set -e  # exit on error
# set -o allexport; source .env; set +o allexport

export DEBUG=''

cd connection
go test -v -run 'TestConnection'
cd -

cd iop
go test -timeout 5m -v -run 'TestParseDate|TestDetectDelimiter|TestFIX|TestConstraints|TestDuckDb|TestParquetDuckDb|TestIcebergReader|TestDeltaReader|TestPartition|TestExtractPartitionTimeValue|TestGetLowestPartTimeUnit|TestMatchedPartitionMask|TestGeneratePartURIsFromRange'
cd -

cd database
go test -v -run 'TestParseTableName|TestRegexMatch|TestParseColumnName'
go test -run TestChunkByColumnRange
cd -

cd filesys
go test -v -run 'TestFileSysLocalCsv|TestFileSysLocalJson|TestFileSysLocalParquet|TestFileSysLocalFormat|TestFileSysGoogle|TestFileSysS3|TestFileSysAzure|TestFileSysSftp|TestFileSysFtp|TestExcel|TestFileSysLocalIceberg|TestFileSysLocalDelta'
cd -
