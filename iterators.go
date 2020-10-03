package parquet

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/bits"

	"github.com/segmentio/parquet/internal/debug"
	pthrift "github.com/segmentio/parquet/internal/gen-go/parquet"
	"github.com/segmentio/parquet/internal/thrift"
)

// ColumnIterator iterates over all the values of a given column across all
// rowGroups.
//type ColumnIterator struct {
//	r        *File
//	path     []string
//	callback PageCallbackFn //TODO leaky
//
//	err                    error
//	rowGroupIterator       *RowGroupIterator
//	rowGroupColumnIterator *RowGroupColumnReader
//}
//
//func (c *ColumnIterator) Next() bool {
//	if c.err != nil {
//		return false
//	}
//	if c.rowGroupIterator == nil {
//		c.rowGroupIterator = &RowGroupIterator{
//			r: c.r,
//		}
//		c.rowGroupIterator.Next()
//	}
//	if c.rowGroupIterator.Value() == nil {
//		return false
//	}
//	if c.rowGroupColumnIterator == nil {
//		c.rowGroupColumnIterator = c.rowGroupIterator.Value().Column(c.path, c.callback)
//		if c.rowGroupColumnIterator == nil {
//			c.err = fmt.Errorf("column not found in row group: %s", c.path)
//			return false
//		}
//	}
//	if !c.rowGroupColumnIterator.Next() {
//		c.rowGroupColumnIterator = nil
//		return c.rowGroupIterator.Next()
//	}
//	return c.rowGroupIterator.Value() != nil
//}
//
//func (c *ColumnIterator) Error() error {
//	if c.rowGroupColumnIterator != nil {
//		return combineErrors(c.err, c.rowGroupColumnIterator.Error())
//	}
//	return c.err
//}
//
//func combineErrors(errors ...error) error {
//	var combined error
//	for _, err := range errors {
//		if err == nil {
//			continue
//		}
//		if combined == nil {
//			combined = err
//			continue
//		}
//		combined = fmt.Errorf("%s; and %w", combined, err)
//	}
//	return combined
//}

// Iterator that goes over each row group of the file.
type RowGroupIterator struct {
	r *File

	rowGroup *RowGroup
	index    int
}

func (i *RowGroupIterator) Next() bool {
	if i.index >= len(i.r.metadata.rowGroups) {
		i.rowGroup = nil
		return false
	}
	i.rowGroup = &RowGroup{
		r:   i.r,
		raw: i.r.metadata.rowGroups[i.index],
	}
	i.index++
	return true
}

func (i *RowGroupIterator) Value() *RowGroup {
	return i.rowGroup
}

type RowGroup struct {
	r   *File
	raw *pthrift.RowGroup
}

// Construct a ColumnIterator for the column at path in schema.
// returns nil if the column does not exist in the schema.
func (rg *RowGroup) Column(path []string) *RowGroupColumnReader {
	s := rg.r.metadata.Schema.At(path...)
	if s == nil {
		return nil
	}
	md := rg.metadataForColumn(path)
	if md == nil {
		return nil
	}
	return &RowGroupColumnReader{
		r:      rg.r.thrift.Fork(),
		schema: s,
		md:     md,
	}
}

func (rg *RowGroup) metadataForColumn(path []string) *pthrift.ColumnMetaData {
	// TODO: build a hashmap of column path -> metadata?
columns:
	for _, col := range rg.raw.Columns {
		md := col.GetMetaData()
		p := md.GetPathInSchema()
		if len(p) != len(path) {
			continue
		}
		for i, el := range p {
			if el != path[i] {
				continue columns
			}
		}
		return md
	}
	return nil
}

// Iterator that goes over every value for a given column across all pages for a
// given RowGroup. Look at ColumnIterator if you want to iterate for all values
// of a column across rowGroups.
type RowGroupColumnReader struct {
	r      *thrift.Reader
	schema *Schema
	md     *pthrift.ColumnMetaData

	ready        bool
	totalRows    int64
	rowsRead     int64
	pageIterator *PageReader
}

func (i *RowGroupColumnReader) Schema() *Schema { return i.schema }

func (i *RowGroupColumnReader) Peek() (Levels, error) {
	err := i.ensurePageAvailable()
	if err != nil {
		return Levels{}, err
	}
	return i.pageIterator.Peek()
}

func (i *RowGroupColumnReader) Read(b RowBuilder) error {
	err := i.ensurePageAvailable()
	if err != nil {
		return err
	}

	err = i.pageIterator.Read(b)
	if err != nil {
		return err
	}
	i.rowsRead++

	return nil
}

// Ensures that the reader has seeked to the beginning of the first page.
func (i *RowGroupColumnReader) ensureReady() error {
	if i.ready {
		return nil
	}
	fileOffset := i.md.GetDataPageOffset() // ignore filepath
	debug.Format("Opening RowGroupColumn at offset %d", fileOffset)

	_, err := i.r.Seek(fileOffset, io.SeekStart)
	if err != nil {
		return err
	}
	i.rowsRead = 0
	i.totalRows = i.md.GetNumValues()
	i.ready = true
	return nil
}

// Ensures the current pageIterator has at least one row available, or asks for
// the next page.
// Returns EOF if all the records have been read.
func (i *RowGroupColumnReader) ensurePageAvailable() error {
	err := i.ensureReady()
	if err != nil {
		return err
	}

	if i.rowsRead >= i.totalRows {
		return EOF
	}

	if i.pageIterator != nil && i.pageIterator.Done() {
		i.pageIterator = nil
	}

	// TODO: that seems odd, why does the PageReader need to be recreated?
	if i.pageIterator == nil {
		codecName := i.md.GetCodec()
		var codec compressionCodec
		switch codecName {
		case pthrift.CompressionCodec_SNAPPY:
			codec = &snappyCodec{}
		default:
			return fmt.Errorf("unknown codec: %s", codecName)
		}
		i.pageIterator = &PageReader{
			r:                i.r,
			schema:           i.schema,
			compressionCodec: codec,
		}
	}
	return nil
}

// EOF indicates the end of the data stream.
var EOF = errors.New("EOF")

type Raw struct {
	Value  interface{}
	Levels Levels
}

// PageReader lazily iterates over values of one page.
type PageReader struct {
	r                *thrift.Reader
	compressionCodec compressionCodec
	schema           *Schema

	valueDecoder           Decoder
	repetitionLevelDecoder Decoder
	definitionLevelDecoder Decoder
	repetitionLevels       []uint32
	definitionLevels       []uint32
	bytes                  []byte
	reader                 io.Reader
	numValues              int32
	valuesRead             int32
	ready                  bool
}

func (p *PageReader) Done() bool {
	return p.valuesRead >= p.numValues
}

// Peek returns error, repetitionLevel, definitionLevel of the next value.
// Returns EOF if there is no more value to read.
// Return any other error encountered during opening the page.
func (p *PageReader) Peek() (Levels, error) {
	levels := Levels{}
	err := p.ensureReady()
	if err != nil {
		return levels, err
	}
	if p.Done() {
		return levels, EOF
	}

	if p.repetitionLevels != nil {
		levels.Repetition = p.repetitionLevels[p.valuesRead]
	}
	if p.definitionLevels != nil {
		levels.Definition = p.definitionLevels[p.valuesRead]
	}

	return levels, nil
}

func (p *PageReader) Read(b RowBuilder) error {
	err := p.ensureReady()
	if err != nil {
		return err
	}

	if p.Done() {
		return EOF
	}

	levels, err := p.Peek()
	if err != nil {
		return err
	}

	if levels.Definition < p.schema.DefinitionLevel {
		err = b.PrimitiveNil(p.schema)
	} else {
		err = b.Primitive(p.schema, p.valueDecoder)
	}
	p.valuesRead++
	return err
}

func (p *PageReader) ensureReady() error {
	if p.ready {
		return nil
	}
	debug.Format("Opening new page")
	// 0. parse the page header
	pageHeader := pthrift.NewPageHeader()
	err := p.r.Unmarshal(pageHeader)
	if err != nil {
		return err
	}
	if pageHeader.GetType() != pthrift.PageType_DATA_PAGE {
		return fmt.Errorf("unsupported page type: %s", pageHeader.GetType())
	}
	p.numValues = pageHeader.GetDataPageHeader().GetNumValues()
	p.valueDecoder, err = decoderFor(pageHeader.GetDataPageHeader().GetEncoding())
	if err != nil {
		return err
	}
	p.repetitionLevelDecoder, err = decoderFor(pageHeader.GetDataPageHeader().GetRepetitionLevelEncoding())
	if err != nil {
		return err
	}
	p.definitionLevelDecoder, err = decoderFor(pageHeader.GetDataPageHeader().GetDefinitionLevelEncoding())
	if err != nil {
		return err
	}

	// 1. decompress the page
	compressedBytesCount := pageHeader.GetCompressedPageSize()
	uncompressedBytesCount := pageHeader.GetUncompressedPageSize()
	// TODO: reuse
	compressedBytes := make([]byte, compressedBytesCount)

	var read int32
	for read < compressedBytesCount {
		var n int
		n, err = p.r.Read(compressedBytes[read:])
		read += int32(n)
		if err != nil {
			return err
		}
	}

	if read != compressedBytesCount {
		return fmt.Errorf("could not read enough compressed bytes")
	}
	// TODO: large buffer. reuse.
	p.bytes = make([]byte, uncompressedBytesCount)
	err = p.compressionCodec.Decode(p.bytes, compressedBytes)
	if err != nil {
		return err
	}
	p.reader = bytes.NewReader(p.bytes)
	p.valueDecoder.prepare(p.reader)

	// 2. maybe parse repetition levels.
	//
	// Repetition levels are skipped when the column is not nested
	// (path = 1). In that case, p.repetitionLevels stays nil, and 0
	// will always be provided to the callback.
	if len(p.schema.Path) > 1 {
		// we need to figure out what is the maximum possible
		// level of repetition so that we can know how many bits
		// at most are required to express repetitions level.
		bitWidth := bits.Len32(p.schema.RepetitionLevel)
		p.repetitionLevels = make([]uint32, p.numValues)
		p.repetitionLevelDecoder.prepare(p.reader)
		err = p.repetitionLevelDecoder.Uint32(bitWidth, p.repetitionLevels)
		if err != nil {
			return err
		}
		if int32(len(p.repetitionLevels)) != p.numValues {
			return fmt.Errorf("expected %d repetition levels, got %d", p.numValues, len(p.repetitionLevels))
		}
	}

	// 3. maybe parse definition levels
	//
	// For data that is required, the definition levels are skipped
	// (if encoded, it will always have the value of the max
	// definition level). In that case, p.definitionLevels stays
	// nil, and 0 will always be provided to the callback.
	if p.schema.DefinitionLevel >= 1 {
		bitWidth := bits.Len32(p.schema.DefinitionLevel)
		p.definitionLevels = make([]uint32, p.numValues)
		p.definitionLevelDecoder.prepare(p.reader)
		err = p.definitionLevelDecoder.Uint32(bitWidth, p.definitionLevels)
		if err != nil {
			return err
		}
	}

	p.ready = true
	return nil
}
