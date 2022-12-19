#!/bin/bash

ret=0

echo building binaries
go build
cd fileserver
go build ./fileserver.go
cd ..

echo starting fileserver
./fileserver/fileserver &
pid=$!
echo fileserver pid is $pid, saving for later

echo running fastar command
./fastar http://localhost:8000/not_found -O > /dev/null
ret=$?

echo checking if errored with exit code 2
if [ $ret -ne 2 ]; then
    echo "Didn't return exit code 2"
    echo $ret
    ret=1
else
    echo "Passed!"    
    ret=0
fi

echo killing fileserver...
kill -9 $pid

exit $ret
