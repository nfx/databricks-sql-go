package dbsql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/databricks/databricks-sql-go/internal/cli_service"
)

type rows struct {
	client               cli_service.TCLIService
	opHandle             *cli_service.TOperationHandle
	pageSize             int64
	location             *time.Location
	fetchResults         *cli_service.TFetchResultsResp
	fetchResultsMetadata *cli_service.TGetResultSetMetadataResp
	nextRowIndex         int64
	nextRowNumber        int64
}

var _ driver.Rows = (*rows)(nil)
var _ driver.RowsColumnTypeScanType = (*rows)(nil)
var _ driver.RowsColumnTypeDatabaseTypeName = (*rows)(nil)
var _ driver.RowsColumnTypeNullable = (*rows)(nil)
var _ driver.RowsColumnTypeLength = (*rows)(nil)

var errRowsFetchPriorToStart error = errors.New("unable to fetch row page prior to start of results")
var errRowsNoSchemaAvailable error = errors.New("no schema in result set metadata response")
var errRowsNoClient error = errors.New("instance of Rows missing client")
var errRowsNilRows error = errors.New("nil Rows instance")

// Columns returns the names of the columns. The number of
// columns of the result is inferred from the length of the
// slice. If a particular column name isn't known, an empty
// string should be returned for that entry.
func (r *rows) Columns() []string {
	err := isValidRows(r)
	if err != nil {
		return []string{}
	}

	resultMetadata, err := r.getResultMetadata()
	if err != nil {
		return []string{}
	}

	if !resultMetadata.IsSetSchema() {
		return []string{}
	}

	tColumns := resultMetadata.Schema.GetColumns()
	colNames := make([]string, len(tColumns))

	for i := range tColumns {
		colNames[i] = tColumns[i].ColumnName
	}

	return colNames
}

// Close closes the rows iterator.
func (r *rows) Close() error {
	err := isValidRows(r)
	if err != nil {
		return err
	}

	req := cli_service.TCloseOperationReq{
		OperationHandle: r.opHandle,
	}

	resp, err := r.client.CloseOperation(context.Background(), &req)
	if err != nil {
		return err
	}
	if err := checkStatus(resp.GetStatus()); err != nil {
		return err
	}

	return nil
}

// Next is called to populate the next row of data into
// the provided slice. The provided slice will be the same
// size as the Columns() are wide.
//
// Next should return io.EOF when there are no more rows.
//
// The dest should not be written to outside of Next. Care
// should be taken when closing Rows not to modify
// a buffer held in dest.
func (r *rows) Next(dest []driver.Value) error {
	err := isValidRows(r)
	if err != nil {
		return err
	}

	// if the next row is not in the current result page
	// fetch the containing page
	if !r.isNextRowInPage() {
		err := r.fetchResultPage()
		if err != nil {
			return err
		}
	}

	// need the column info to retrieve/convert values
	metadata, err := r.getResultMetadata()
	if err != nil {
		return err
	}

	// populate the destinatino slice
	for i := range dest {
		val, err := value(r.fetchResults.Results.Columns[i], metadata.Schema.Columns[i], r.nextRowIndex, r.location)

		if err != nil {
			return err
		}

		dest[i] = val
	}

	r.nextRowIndex++
	r.nextRowNumber++

	return nil
}

// ColumnTypeScanType returns column's native type
func (r *rows) ColumnTypeScanType(index int) reflect.Type {
	err := isValidRows(r)
	if err != nil {
		// TODO: is there a better way to handle this
		return nil
	}

	column, err := r.getColumnMetadataByIndex(index)
	if err != nil {
		// TODO: is there a better way to handle this
		return nil
	}

	scanType := getScanType(column)
	return scanType
}

// ColumnTypeDatabaseTypeName returns column's database type name
func (r *rows) ColumnTypeDatabaseTypeName(index int) string {
	err := isValidRows(r)
	if err != nil {
		// TODO: is there a better way to handle this
		return ""
	}

	column, err := r.getColumnMetadataByIndex(index)
	if err != nil {
		// TODO: is there a better way to handle this
		return ""
	}

	dbtype := getDBTypeName(column)

	return dbtype
}

// ColumnTypeNullable returns a flag indicating whether the column is nullable
// and an ok value of true if the status of the column is known.  Otherwise
// a value of false is returned for ok.
func (r *rows) ColumnTypeNullable(index int) (nullable, ok bool) {
	// TODO: Update if we can figure out this information
	return false, false
}

func (r *rows) ColumnTypeLength(index int) (length int64, ok bool) {
	columnInfo, err := r.getColumnMetadataByIndex(index)
	if err != nil {
		return 0, false
	}

	typeName := getDBTypeID(columnInfo)
	// TODO: figure out how to get better metadata about complex types
	// currently map, array, and struct are returned as strings
	switch typeName {
	case cli_service.TTypeId_STRING_TYPE,
		cli_service.TTypeId_VARCHAR_TYPE,
		cli_service.TTypeId_BINARY_TYPE,
		cli_service.TTypeId_ARRAY_TYPE,
		cli_service.TTypeId_MAP_TYPE,
		cli_service.TTypeId_STRUCT_TYPE:
		return math.MaxInt64, true
	default:
		return 0, false
	}
}

var (
	scanTypeNull     = reflect.TypeOf(nil)
	scanTypeBoolean  = reflect.TypeOf(true)
	scanTypeFloat32  = reflect.TypeOf(float32(0))
	scanTypeFloat64  = reflect.TypeOf(float64(0))
	scanTypeInt8     = reflect.TypeOf(int8(0))
	scanTypeInt16    = reflect.TypeOf(int16(0))
	scanTypeInt32    = reflect.TypeOf(int32(0))
	scanTypeInt64    = reflect.TypeOf(int64(0))
	scanTypeString   = reflect.TypeOf("")
	scanTypeDateTime = reflect.TypeOf(time.Time{})
	scanTypeRawBytes = reflect.TypeOf(sql.RawBytes{})
	scanTypeUnknown  = reflect.TypeOf(new(interface{}))
)

func getScanType(column *cli_service.TColumnDesc) reflect.Type {

	// TODO: handle non-primitive types
	entry := column.TypeDesc.Types[0].PrimitiveEntry

	switch entry.Type {
	case cli_service.TTypeId_BOOLEAN_TYPE:
		return scanTypeBoolean
	case cli_service.TTypeId_TINYINT_TYPE:
		return scanTypeInt8
	case cli_service.TTypeId_SMALLINT_TYPE:
		return scanTypeInt16
	case cli_service.TTypeId_INT_TYPE:
		return scanTypeInt32
	case cli_service.TTypeId_BIGINT_TYPE:
		return scanTypeInt64
	case cli_service.TTypeId_FLOAT_TYPE:
		return scanTypeFloat32
	case cli_service.TTypeId_DOUBLE_TYPE:
		return scanTypeFloat64
	case cli_service.TTypeId_NULL_TYPE:
		return scanTypeNull
	case cli_service.TTypeId_STRING_TYPE:
		return scanTypeString
	case cli_service.TTypeId_CHAR_TYPE:
		return scanTypeString
	case cli_service.TTypeId_VARCHAR_TYPE:
		return scanTypeString
	case cli_service.TTypeId_DATE_TYPE, cli_service.TTypeId_TIMESTAMP_TYPE:
		return scanTypeDateTime
	case cli_service.TTypeId_DECIMAL_TYPE, cli_service.TTypeId_BINARY_TYPE, cli_service.TTypeId_ARRAY_TYPE,
		cli_service.TTypeId_STRUCT_TYPE, cli_service.TTypeId_MAP_TYPE, cli_service.TTypeId_UNION_TYPE:
		return scanTypeRawBytes
	case cli_service.TTypeId_USER_DEFINED_TYPE:
		return scanTypeUnknown
	case cli_service.TTypeId_INTERVAL_DAY_TIME_TYPE, cli_service.TTypeId_INTERVAL_YEAR_MONTH_TYPE:
		return scanTypeString
	default:
		return scanTypeUnknown
	}
}

func getDBTypeName(column *cli_service.TColumnDesc) string {
	// TODO: handle non-primitive types
	entry := column.TypeDesc.Types[0].PrimitiveEntry
	dbtype := strings.TrimSuffix(entry.Type.String(), "_TYPE")

	return dbtype
}

func getDBTypeID(column *cli_service.TColumnDesc) cli_service.TTypeId {
	// TODO: handle non-primitive types
	entry := column.TypeDesc.Types[0].PrimitiveEntry
	return entry.Type
}

// isValidRows checks that the row instance is not nil
// and that it has a client
func isValidRows(r *rows) error {
	if r == nil {
		return errRowsNilRows
	}

	if r.client == nil {
		return errRowsNoClient
	}

	return nil
}

func (r *rows) getColumnMetadataByIndex(index int) (*cli_service.TColumnDesc, error) {
	err := isValidRows(r)
	if err != nil {
		return nil, err
	}

	resultMetadata, err := r.getResultMetadata()
	if err != nil {
		return nil, err
	}

	if !resultMetadata.IsSetSchema() {
		return nil, errRowsNoSchemaAvailable
	}

	columns := resultMetadata.GetSchema().GetColumns()
	if index < 0 || index >= len(columns) {
		return nil, fmt.Errorf("invalid column index: %d", index)
	}

	// tColumns := resultMetadata.Schema.GetColumns()
	return columns[index], nil
}

// isNextRowInPage returns a boolean flag indicating whether
// the next result set row is in the current result set page
func (r *rows) isNextRowInPage() bool {
	if r == nil || r.fetchResults == nil {
		return false
	}

	nRowsInPage := getNRows(r.fetchResults.GetResults())
	if nRowsInPage == 0 {
		return false
	}

	startRowOffset := r.getPageStartRowNum()
	return r.nextRowNumber >= startRowOffset && r.nextRowNumber < (startRowOffset+nRowsInPage)
}

func (r *rows) getResultMetadata() (*cli_service.TGetResultSetMetadataResp, error) {
	if r.fetchResultsMetadata == nil {
		err := isValidRows(r)
		if err != nil {
			return nil, err
		}

		req := cli_service.TGetResultSetMetadataReq{
			OperationHandle: r.opHandle,
		}

		resp, err := r.client.GetResultSetMetadata(context.Background(), &req)
		if err != nil {
			return nil, err
		}

		if err := checkStatus(resp.GetStatus()); err != nil {
			return nil, err
		}

		r.fetchResultsMetadata = resp

	}

	return r.fetchResultsMetadata, nil
}

func (r *rows) fetchResultPage() error {
	err := isValidRows(r)
	if err != nil {
		return err
	}

	for !r.isNextRowInPage() {

		// determine the direction of page fetching.  Currently we only handle
		// TFetchOrientation_FETCH_PRIOR and TFetchOrientation_FETCH_NEXT
		var direction cli_service.TFetchOrientation = r.getPageFetchDirection()
		if direction == cli_service.TFetchOrientation_FETCH_PRIOR {
			if r.getPageStartRowNum() == 0 {
				return errRowsFetchPriorToStart
			}
		} else if direction == cli_service.TFetchOrientation_FETCH_NEXT {
			if r.fetchResults != nil && !r.fetchResults.GetHasMoreRows() {
				return io.EOF
			}
		} else {
			return fmt.Errorf("unhandled fetch result orientation: %s", direction)
		}

		req := cli_service.TFetchResultsReq{
			OperationHandle: r.opHandle,
			MaxRows:         r.pageSize,
			Orientation:     direction,
		}

		fetchResult, err := r.client.FetchResults(context.Background(), &req)
		if err != nil {
			return err
		}

		r.fetchResults = fetchResult
	}

	// don't assume the next row is the first row in the page
	r.nextRowIndex = r.nextRowNumber - r.getPageStartRowNum()

	return nil
}

// getPageFetchDirection returns the cli_service.TFetchOrientation
// necessary to fetch a result page containing the next row number.
// Note: if the next row number is in the current page TFetchOrientation_FETCH_NEXT
// is returned. Use rows.nextRowInPage to determine if a fetch is necessary
func (r *rows) getPageFetchDirection() cli_service.TFetchOrientation {
	if r == nil {
		return cli_service.TFetchOrientation_FETCH_NEXT
	}

	if r.nextRowNumber < r.getPageStartRowNum() {
		return cli_service.TFetchOrientation_FETCH_PRIOR
	}

	return cli_service.TFetchOrientation_FETCH_NEXT
}

// getPageStartRowNum returns an int64 value which is the
// starting row number of the current result page, -1 is returned
// if there is no result page
func (r *rows) getPageStartRowNum() int64 {
	if r == nil || r.fetchResults == nil || r.fetchResults.GetResults() == nil {
		return 0
	}

	return r.fetchResults.GetResults().GetStartRowOffset()
}

func checkStatus(status *cli_service.TStatus) error {
	if status.StatusCode == cli_service.TStatusCode_ERROR_STATUS {
		return errors.New(status.GetErrorMessage())
	}

	if status.StatusCode == cli_service.TStatusCode_INVALID_HANDLE_STATUS {
		return errors.New("thrift: invalid handle")
	}

	return nil
}

const (
	// TimestampFormat is JDBC compliant timestamp format
	TimestampFormat = "2006-01-02 15:04:05.999999999"
	DateFormat      = "2006-01-02"
)

func value(tColumn *cli_service.TColumn, tColumnDesc *cli_service.TColumnDesc, rowNum int64, location *time.Location) (val interface{}, err error) {
	if location == nil {
		location = time.UTC
	}

	entry := tColumnDesc.TypeDesc.Types[0].PrimitiveEntry
	dbtype := strings.TrimSuffix(entry.Type.String(), "_TYPE")
	if tVal := tColumn.GetStringVal(); tVal != nil && !isNull(tVal.Nulls, rowNum) {
		val = tVal.Values[rowNum]
		if dbtype == "TIMESTAMP" {
			t, err := time.ParseInLocation(TimestampFormat, val.(string), location)
			if err == nil {
				val = t
			}
		} else if dbtype == "DATE" {
			t, err := time.ParseInLocation(DateFormat, val.(string), location)
			if err == nil {
				val = t
			}
		}
	} else if tVal := tColumn.GetByteVal(); tVal != nil && !isNull(tVal.Nulls, rowNum) {
		val = tVal.Values[rowNum]
	} else if tVal := tColumn.GetI16Val(); tVal != nil && !isNull(tVal.Nulls, rowNum) {
		val = tVal.Values[rowNum]
	} else if tVal := tColumn.GetI32Val(); tVal != nil && !isNull(tVal.Nulls, rowNum) {
		val = tVal.Values[rowNum]
	} else if tVal := tColumn.GetI64Val(); tVal != nil && !isNull(tVal.Nulls, rowNum) {
		val = tVal.Values[rowNum]
	} else if tVal := tColumn.GetBoolVal(); tVal != nil && !isNull(tVal.Nulls, rowNum) {
		val = tVal.Values[rowNum]
	} else if tVal := tColumn.GetDoubleVal(); tVal != nil && !isNull(tVal.Nulls, rowNum) {
		val = tVal.Values[rowNum]
	} else if tVal := tColumn.GetBinaryVal(); tVal != nil && !isNull(tVal.Nulls, rowNum) {
		val = tVal.Values[rowNum]
	}

	return val, err
}

func isNull(nulls []byte, position int64) bool {
	index := position / 8
	if int64(len(nulls)) > index {
		b := nulls[index]
		return (b & (1 << (uint)(position%8))) != 0
	}
	return false
}

func getNRows(rs *cli_service.TRowSet) int64 {
	if rs == nil {
		return 0
	}
	for _, col := range rs.Columns {
		if col.BoolVal != nil {
			return int64(len(col.BoolVal.Values))
		}
		if col.ByteVal != nil {
			return int64(len(col.ByteVal.Values))
		}
		if col.I16Val != nil {
			return int64(len(col.I16Val.Values))
		}
		if col.I32Val != nil {
			return int64(len(col.I32Val.Values))
		}
		if col.I64Val != nil {
			return int64(len(col.I64Val.Values))
		}
		if col.StringVal != nil {
			return int64(len(col.StringVal.Values))
		}
		if col.DoubleVal != nil {
			return int64(len(col.DoubleVal.Values))
		}
		if col.BinaryVal != nil {
			return int64(len(col.BinaryVal.Values))
		}
	}
	return 0
}