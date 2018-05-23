package gohive

/*
rowset.go comes from github.com/derekgr/hivething, tiny changes
*/
import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"reflect"

	"git.apache.org/thrift.git/lib/go/thrift"
	"github.com/eaciit/toolkit"
	"github.com/lwldcr/gohive/tcliservice"
)

// Options for opened Hive sessions.
type Options struct {
	PollIntervalSeconds int64
	BatchSize           int64
}

var (
	DefaultOptions = Options{PollIntervalSeconds: 5, BatchSize: 10000}
)

type RowSetR struct {
	thrift    *tcliservice.TCLIServiceClient
	operation *tcliservice.TOperationHandle
	options   Options

	columns    []*tcliservice.TColumnDesc
	columnStrs []string

	offset  int
	rowSet  *tcliservice.TRowSet
	hasMore bool
	ready   bool

	nextRow []interface{}
}

// A RowSet represents an asyncronous hive operation. You can
// Reattach to a previously submitted hive operation if you
// have a valid thrift client, and the serialized Handle()
// from the prior operation.
type RowSet interface {
	Handle() ([]byte, error)
	Columns() []string
	Next() bool
	Scan(dest ...interface{}) error
	ScanObject(dest interface{}) error
	Poll() (*Status, error)
	Wait() (*Status, error)
	Close() error
}

// Represents job status, including success state and time the
// status was updated.
type Status struct {
	state *tcliservice.TOperationState
	Error error
	At    time.Time
}

func newRowSet(thrift *tcliservice.TCLIServiceClient, operation *tcliservice.TOperationHandle, options Options) RowSet {
	return &RowSetR{thrift, operation, options, nil, nil, 0, nil, true, false, nil}
}

// Construct a RowSet for a previously submitted operation, using the prior operation's Handle()
// and a valid thrift client to a hive service that is aware of the operation.
func Reattach(conn *TSaslClientTransport, handle []byte) (RowSet, error) {
	operation, err := deserializeOp(handle)
	if err != nil {
		return nil, err
	}

	return newRowSet(conn.Client, operation, conn.options), nil
}

// Issue a thrift call to check for the job's current status.
func (r *RowSetR) Poll() (*Status, error) {
	req := tcliservice.NewTGetOperationStatusReq()
	req.OperationHandle = r.operation

	resp, err := r.thrift.GetOperationStatus(req)
	if err != nil {
		return nil, fmt.Errorf("Error getting status: %+v, %v", resp, err)
	}

	if !isSuccessStatus(*resp.Status) {
		return nil, fmt.Errorf("GetStatus call failed: %s", resp.Status.String())
	}

	if resp.OperationState == nil {
		return nil, errors.New("No error from GetStatus, but nil status!")
	}

	return &Status{resp.OperationState, nil, time.Now()}, nil
}

// Wait until the job is complete, one way or another, returning Status and error.
func (r *RowSetR) Wait() (*Status, error) {
	for {
		status, err := r.Poll()

		if err != nil {
			return nil, err
		}

		if status.IsComplete() {
			if status.IsSuccess() {
				// Fetch operation metadata.
				metadataReq := tcliservice.NewTGetResultSetMetadataReq()
				metadataReq.OperationHandle = r.operation

				metadataResp, err := r.thrift.GetResultSetMetadata(metadataReq)
				if err != nil {
					return nil, err
				}

				if !isSuccessStatus(*metadataResp.Status) {
					return nil, fmt.Errorf("GetResultSetMetadata failed: %s", metadataResp.Status.String())
				}

				r.columns = metadataResp.Schema.Columns
				r.ready = true

				return status, nil
			}
			return nil, fmt.Errorf("Query failed execution: %s", status.state.String())
		}

		time.Sleep(time.Duration(r.options.PollIntervalSeconds) * time.Second)
	}
}

func (r *RowSetR) waitForSuccess() error {
	if !r.ready {
		status, err := r.Wait()
		if err != nil {
			return err
		}
		if !status.IsSuccess() || !r.ready {
			return fmt.Errorf("Unsuccessful query execution: %+v", status)
		}
	}

	return nil
}

// Prepares a row for scanning into memory, by reading data from hive if
// the operation is successful, blocking until the operation is
// complete, if necessary.
// Returns true is a row is available to Scan(), and false if the
// results are empty or any other error occurs.
func (r *RowSetR) Next() bool {
	if err := r.waitForSuccess(); err != nil {
		return false
	}

	if r.rowSet == nil || r.offset >= len(r.rowSet.Rows) {
		if !r.hasMore {
			return false
		}

		fetchReq := tcliservice.NewTFetchResultsReq()
		fetchReq.OperationHandle = r.operation
		fetchReq.Orientation = tcliservice.TFetchOrientation_FETCH_NEXT
		fetchReq.MaxRows = r.options.BatchSize

		resp, err := r.thrift.FetchResults(fetchReq)
		if err != nil {
			log.Printf("FetchResults failed: %v\n", err)
			return false
		}

		if !isSuccessStatus(*resp.Status) {
			log.Printf("FetchResults failed: %s\n", resp.Status.String())
			return false
		}

		r.offset = 0
		r.rowSet = resp.Results
		r.hasMore = *resp.HasMoreRows
	}
	if r.offset >= len(r.rowSet.Rows) {
		return false
	}

	for {
		row := r.rowSet.Rows[r.offset]
		r.nextRow = make([]interface{}, len(r.Columns()))
		fmt.Println("Rows:", len(r.rowSet.Rows))
		fmt.Println("Offset:", r.offset)

		if err := convertRow(row, r.nextRow); err != nil {
			fmt.Println(fmt.Sprintf("Error converting row: %v", err))
			if r.offset >= len(r.rowSet.Rows) - 1 {
				return false
			}
			r.offset++
			continue
		}
		r.offset++
		break
	}




	return true
}

// Scan the last row prepared via Next() into the destination(s) provided,
// which must be pointers to value types, as in database.sql. Further,
// only pointers of the following types are supported:
// 	- int, int16, int32, int64
// 	- string, []byte
// 	- float64
//	 - bool
func (r *RowSetR) Scan(dest ...interface{}) error {
	// TODO: Add type checking and conversion between compatible
	// types where possible, as well as some common error checking,
	// like passing nil. database/sql's method is very convenient,
	// for example: http://golang.org/src/pkg/database/sql/convert.go, like 85
	if r.nextRow == nil {
		return errors.New("No row to scan! Did you call Next() first?")
	}

	if len(dest) != len(r.nextRow) {
		return fmt.Errorf("Can't scan into %d arguments with input of length %d", len(dest), len(r.nextRow))
	}

	for i, val := range r.nextRow {
		d := dest[i]
		switch dt := d.(type) {
		case *string:
			switch st := val.(type) {
			case string:
				*dt = st
			default:
				*dt = fmt.Sprintf("%v", val)
			}
		case *[]byte:
			*dt = []byte(val.(string))
		case *int64:
			*dt = val.(int64)
		case *int32:
			*dt = val.(int32)
		case *int:
			*dt = int(val.(int32))
		case *int16:
			*dt = val.(int16)
		case *float64:
			*dt = val.(float64)
		case *bool:
			*dt = val.(bool)
		case *interface{}:
			*dt = val
		default:
			return fmt.Errorf("Can't scan value of type %T with value %v", dt, val)
		}
	}

	return nil
}

// just as its name implied, DO NOT PASS LIST/SLICE INTO THIS FUNCTION
// fetch single object from database
func (r *RowSetR) ScanObject(m interface{}) error {
	if r.nextRow == nil {
		return errors.New("No row to scan! Did you call Next() first?")
	}

	var valueType reflect.Type
	valueType = reflect.TypeOf(m).Elem()
	dataTypeList := toolkit.M{}

	if valueType.Kind() == reflect.Struct {
		for i := 0; i < valueType.NumField(); i++ {
			namaField := strings.ToLower(valueType.Field(i).Name)
			dataType := valueType.Field(i).Type.String()
			dataTypeList.Set(namaField, dataType)
		}
	}
	columns := r.Columns()
	for i, _ := range columns {
		yy := strings.Split(columns[i], ".")
		if len(yy) == 2 {
			columns[i] = yy[1]
		}
	}
	count := len(columns)
	values := make([]interface{}, count)
	valuePtrs := make([]interface{}, count)
	for i := 0; i < count; i++ {
		valuePtrs[i] = &values[i]
	}
	r.Scan(valuePtrs...)
	//fmt.Println(columns)
	//fmt.Println(values)
	entry := toolkit.M{}
	for i, col := range columns {
		var v interface{}
		val := values[i]
		var ok bool
		var b []byte
		if val == nil {
			v = nil
		} else {
			b, ok = val.([]byte)
			if ok {
				v = string(b)
			} else {
				v = val
			}
		}
		v = structValue(dataTypeList, col, v)
		if v != nil {
			entry.Set(strings.ToLower(col), v)
		}
	}
	toolkit.Serde(entry, m, "json")
	return nil
}
func structValue(dataTypeList toolkit.M, col string, v interface{}) interface{} {
	for fieldname, datatype := range dataTypeList {
		if strings.ToLower(col) == fieldname {
			switch datatype.(string) {
			case "time.Time":
				val, e := time.Parse(time.RFC3339, toolkit.ToString(v))
				if e != nil {
					v = toolkit.ToString(v)
				} else {
					v = val
				}
			case "int", "int32", "int64":
				val, e := strconv.Atoi(toolkit.ToString(v))
				if e != nil {
					v = toolkit.ToString(v)
				} else {
					v = val
				}
			case "float", "float32", "float64":
				val, e := strconv.ParseFloat(toolkit.ToString(v), 64)
				if e != nil {
					v = toolkit.ToString(v)
				} else {
					v = val
				}
			case "bool":
				val, e := strconv.ParseBool(toolkit.ToString(v))
				if e != nil {
					v = toolkit.ToString(v)
				} else {
					v = val
				}
			default:
				v = toolkit.ToString(v)
			}

		}
	}
	return v
}

// Returns the names of the columns for the given operation,
// blocking if necessary until the information is available.
func (r *RowSetR) Columns() []string {
	if r.columnStrs == nil {
		if err := r.waitForSuccess(); err != nil {
			return nil
		}

		ret := make([]string, len(r.columns))
		for i, col := range r.columns {
			ret[i] = col.ColumnName
		}

		r.columnStrs = ret
	}

	return r.columnStrs
}

// Return a serialized representation of an identifier that can later
// be used to reattach to a running operation. This identifier and
// serialized representation should be considered opaque by users.
func (r *RowSetR) Handle() ([]byte, error) {
	return serializeOp(r.operation)
}

func convertRow(row *tcliservice.TRow, dest []interface{}) error {
	if len(row.ColVals) != len(dest) {
		return fmt.Errorf("Returned row has %d values, but scan row has %d", len(row.ColVals), len(dest))
	}

	for i, col := range row.ColVals {
		val, err := convertColumn(col)
		if err != nil {
			return fmt.Errorf("Error converting column %d: %v", i, err)
		}
		dest[i] = val
	}

	return nil
}

func convertColumn(col *tcliservice.TColumnValue) (interface{}, error) {
	switch {
	case col.StringVal != nil && col.StringVal.IsSetValue():
		return col.StringVal.GetValue(), nil
	case col.BoolVal != nil && col.BoolVal.IsSetValue():
		return col.BoolVal.GetValue(), nil
	case col.ByteVal != nil && col.ByteVal.IsSetValue():
		return int64(col.ByteVal.GetValue()), nil
	case col.I16Val != nil && col.I16Val.IsSetValue():
		return int32(col.I16Val.GetValue()), nil
	case col.I32Val != nil && col.I32Val.IsSetValue():
		return col.I32Val.GetValue(), nil
	case col.I64Val != nil && col.I64Val.IsSetValue():
		return col.I64Val.GetValue(), nil
	case col.DoubleVal != nil && col.DoubleVal.IsSetValue():
		return col.DoubleVal.GetValue(), nil
	default:
		return nil, fmt.Errorf("Can't convert column value %v", col)
	}
}

// Returns a string representation of operation status.
func (s Status) String() string {
	if s.state == nil {
		return "unknown"
	}
	return s.state.String()
}

// Returns true if the job has completed or failed.
func (s Status) IsComplete() bool {
	if s.state == nil {
		return false
	}

	switch *s.state {
	case tcliservice.TOperationState_FINISHED_STATE,
		tcliservice.TOperationState_CANCELED_STATE,
		tcliservice.TOperationState_CLOSED_STATE,
		tcliservice.TOperationState_ERROR_STATE:
		return true
	}

	return false
}

// Returns true if the job compelted successfully.
func (s Status) IsSuccess() bool {
	if s.state == nil {
		return false
	}

	return *s.state == tcliservice.TOperationState_FINISHED_STATE
}

func deserializeOp(handle []byte) (*tcliservice.TOperationHandle, error) {
	ser := thrift.NewTDeserializer()
	var val tcliservice.TOperationHandle

	if err := ser.Read(&val, handle); err != nil {
		return nil, err
	}

	return &val, nil
}

func serializeOp(operation *tcliservice.TOperationHandle) ([]byte, error) {
	ser := thrift.NewTSerializer()
	return ser.Write(operation)
}

// Close do close operation
func (r *RowSetR) Close() error {
	closeOperationReq := tcliservice.NewTCloseOperationReq()
	closeOperationReq.OperationHandle = r.operation

	_, err := r.thrift.CloseOperation(closeOperationReq)

	return err
}
