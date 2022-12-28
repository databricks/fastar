#!/bin/bash

ret=0

echo building binaries
go build
cd fileserver
go build ./fileserver.go
cd ..

echo generating download file in /tmp
dd if=/dev/urandom of=/tmp/foo bs=1k count=$((1<<20))

echo starting fileserver
./fileserver/fileserver &
pid=$!
echo fileserver pid is $pid, saving for later

echo running fastar command
./fastar http://localhost:8000/foo --min-speed 4096M -O > /dev/null
ret=$?

echo checking if errored with exit code 5
if [ $ret -ne 5 ]; then
    echo "Didn't return exit code 5"
    echo $ret
    ret=1
else
    echo "Passed!"    
    ret=0
fi

echo killing fileserver...
kill -9 $pid

exit $ret
