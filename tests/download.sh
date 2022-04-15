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

chunkSize=$((1<<20))
multipartSize=$((4<<20))

for fileSize in 0 1 $(($chunkSize-1)) $chunkSize $(($chunkSize+1)) $(($multipartSize-1)) $(($multipartSize)) $(($multipartSize+1))
do
    echo testing with fileSize $fileSize
    rm -rf /tmp/source
    rm -rf /tmp/download
    dd if=/dev/urandom of=/tmp/source bs=1 count=$fileSize
    ./fastar http://localhost:8000/source --chunk-size 1 --download-workers 4 > /tmp/download
    if diff /tmp/source /tmp/download; then
        echo files match
    else
        echo xxx files do not match
        ret=1
    fi
done

echo killing fileserver...
kill -9 $pid

exit $ret
