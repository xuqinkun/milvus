## Binlog

InsertBinlog、DeleteBinlog、DDLBinlog

Binlog is stored in a columnar storage format, every column in schema is stored in an individual file.
Timestamp, schema, row id and primary key allocated by system are four special columns.
Schema column records the DDL of the collection.


## Event format

Binlog file consists of 4 bytes magic number and a series of events. The first event must be descriptor event.

### Event format

```
+=====================================+=====================================================================+
| event  | Timestamp         0 : 8    | create timestamp                                                    |
| header +----------------------------+---------------------------------------------------------------------+
|        | TypeCode          8 : 1    | event type code                                                     |
|        +----------------------------+---------------------------------------------------------------------+
|        | ServerID          9 : 4    | write node id                                                       |
|        +----------------------------+---------------------------------------------------------------------+
|        | EventLength      13 : 4    | length of event, including header and data                          |
|        +----------------------------+---------------------------------------------------------------------+
|        | NextPosition     17 : 4    | offset of next event from the start of file                         |
+=====================================+=====================================================================+
| event  | fixed part       21 : x    |                                                                     |
| data   +----------------------------+---------------------------------------------------------------------+
|        | variable part              |                                                                     |
+=====================================+=====================================================================+
```


### Descriptor Event format

```
+=====================================+=====================================================================+
| event  | Timestamp         0 : 8    | create timestamp                                                    |
| header +----------------------------+---------------------------------------------------------------------+
|        | TypeCode          8 : 1    | event type code                                                     |
|        +----------------------------+---------------------------------------------------------------------+
|        | ServerID          9 : 4    | write node id                                                       |
|        +----------------------------+---------------------------------------------------------------------+
|        | EventLength      13 : 4    | length of event, including header and data                          |
|        +----------------------------+---------------------------------------------------------------------+
|        | NextPosition     17 : 4    | offset of next event from the start of file                         |
+=====================================+=====================================================================+
| event  | BinlogVersion    21 : 2    | binlog version                                                      |
| data   +----------------------------+---------------------------------------------------------------------+
|        | ServerVersion    23 : 8    | write node version                                                  |
|        +----------------------------+---------------------------------------------------------------------+
|        | CommitID         31 : 8    | commit id of the programe in git                                    |
|        +----------------------------+---------------------------------------------------------------------+
|        | HeaderLength     39 : 1    | header length of other event                                        |
|        +----------------------------+---------------------------------------------------------------------+
|        | CollectionID     40 : 8    | collection id                                                       |
|        +----------------------------+---------------------------------------------------------------------+
|        | PartitionID      48 : 8    | partition id (schema column does not need)                          |
|        +----------------------------+---------------------------------------------------------------------+
|        | SegmentID        56 : 8    | segment id (schema column does not need)                            |
|        +----------------------------+---------------------------------------------------------------------+
|        | StartTimestamp   64 : 1    | minimum timestamp allocated by master of all events in this file    |
|        +----------------------------+---------------------------------------------------------------------+
|        | EndTimestamp     65 : 1    | maximum timestamp allocated by master of all events in this file    |
|        +----------------------------+---------------------------------------------------------------------+
|        | PayloadDataType  66 : 1    | data type of payload                                                |
|        +----------------------------+---------------------------------------------------------------------+
|        | PostHeaderLength 67 : n    | header lengths for all event types                                  |
+=====================================+=====================================================================|
```


### Type code

```
DESCRIPTOR_EVENT
INSERT_EVENT
DELETE_EVENT
CREATE_COLLECTION_EVENT
DROP_COLLECTION_EVENT
CREATE_PARTITION_EVENT
DROP_PARTITION_EVENT
```

DESCRIPTOR_EVENT must appear in all column files and always be the first event.

INSERT_EVENT 可以出现在除 DDL binlog 文件外的其他列的 binlog

DELETE_EVENT 只能用于 primary key 的 binlog 文件（目前只有按照 primary key 删除）

CREATE_COLLECTION_EVENT、DROP_COLLECTION_EVENT、CREATE_PARTITION_EVENT、DROP_PARTITION_EVENT 只出现在 DDL binlog 文件


### Event data part

```
event data part

INSERT_EVENT:
+================================================+==========================================================+
| event  | fixed  |  StartTimestamp      x : 8   | min timestamp in this event                              |
| data   | part   +------------------------------+----------------------------------------------------------+
|        |        |  EndTimestamp      x+8 : 8   | max timestamp in this event                              |
|        |        +------------------------------+----------------------------------------------------------+
|        |        |  reserved         x+16 : y   | reserved part                                            |
|        +--------+------------------------------+----------------------------------------------------------+
|        |variable|  parquet payload             | payload in parquet format                                |
|        |part    |                              |                                                          |
+================================================+==========================================================+

other events are similar with INSERT_EVENT
```


### Example

Schema

​	string | int | float(optional) | vector(512)



Request:

​	InsertRequest  rows(1W)

​	DeleteRequest pk=1

​	DropPartition partitionTag="abc"



insert binlogs:

​	rowid, pk, ts, string, int, float, vector 6 files

​	all events are INSERT_EVENT
​	float column file contains some NULL value

delete binlogs:

​	pk, ts 2 files

​	pk's events are DELETE_EVENT, ts's events are INSERT_EVENT

DDL binlogs:

​	ddl, ts

​	ddl's event is DROP_PARTITION_EVENT, ts's event is INSERT_EVENT



C++ interface

```c++
typedef void* CPayloadWriter
typedef struct CBuffer {
  char* data;
  int length;
} CBuffer

typedef struct CStatus {
  int error_code;
  const char* error_msg;
} CStatus


// C++ interface
// writer
CPayloadWriter NewPayloadWriter(int columnType);
CStatus AddBooleanToPayload(CPayloadWriter payloadWriter, bool *values, int length);
CStatus AddInt8ToPayload(CPayloadWriter payloadWriter, int8_t *values, int length);
CStatus AddInt16ToPayload(CPayloadWriter payloadWriter, int16_t *values, int length);
CStatus AddInt32ToPayload(CPayloadWriter payloadWriter, int32_t *values, int length);
CStatus AddInt64ToPayload(CPayloadWriter payloadWriter, int64_t *values, int length);
CStatus AddFloatToPayload(CPayloadWriter payloadWriter, float *values, int length);
CStatus AddDoubleToPayload(CPayloadWriter payloadWriter, double *values, int length);
CStatus AddOneStringToPayload(CPayloadWriter payloadWriter, char *cstr, int str_size);
CStatus AddBinaryVectorToPayload(CPayloadWriter payloadWriter, uint8_t *values, int dimension, int length);
CStatus AddFloatVectorToPayload(CPayloadWriter payloadWriter, float *values, int dimension, int length);

CStatus FinishPayloadWriter(CPayloadWriter payloadWriter);
CBuffer GetPayloadBufferFromWriter(CPayloadWriter payloadWriter);
int GetPayloadLengthFromWriter(CPayloadWriter payloadWriter);
CStatus ReleasePayloadWriter(CPayloadWriter handler);

// reader
CPayloadReader NewPayloadReader(int columnType, uint8_t *buffer, int64_t buf_size);
CStatus GetBoolFromPayload(CPayloadReader payloadReader, bool **values, int *length);
CStatus GetInt8FromPayload(CPayloadReader payloadReader, int8_t **values, int *length);
CStatus GetInt16FromPayload(CPayloadReader payloadReader, int16_t **values, int *length);
CStatus GetInt32FromPayload(CPayloadReader payloadReader, int32_t **values, int *length);
CStatus GetInt64FromPayload(CPayloadReader payloadReader, int64_t **values, int *length);
CStatus GetFloatFromPayload(CPayloadReader payloadReader, float **values, int *length);
CStatus GetDoubleFromPayload(CPayloadReader payloadReader, double **values, int *length);
CStatus GetOneStringFromPayload(CPayloadReader payloadReader, int idx, char **cstr, int *str_size);
CStatus GetBinaryVectorFromPayload(CPayloadReader payloadReader, uint8_t **values, int *dimension, int *length);
CStatus GetFloatVectorFromPayload(CPayloadReader payloadReader, float **values, int *dimension, int *length);

int GetPayloadLengthFromReader(CPayloadReader payloadReader);
CStatus ReleasePayloadReader(CPayloadReader payloadReader);
```
