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

echo generating source files
rm -rf /tmp/rand*
dd if=/dev/urandom of=/tmp/rand bs=1M count=10
# canonical file types
tar -cf /tmp/rand.tar /tmp/rand
gzip -c -k /tmp/rand.tar > /tmp/rand.tar.gz
lz4 /tmp/rand.tar /tmp/rand.tar.lz4
# missing extensions
cp /tmp/rand.tar /tmp/randTAR
cp /tmp/rand.tar.gz /tmp/randTARGZ
cp /tmp/rand.tar.lz4 /tmp/randTARLZ4

rm -rf /tmp/output
mkdir /tmp/output

filenames=(rand.tar rand.tar.gz rand.tar.lz4 randTAR randTARGZ randTARLZ4)
compressions=(tar gzip lz4 tar gzip lz4)

echo testing compression inference
for ((i = 0; i < ${#filenames[@]}; ++i))
do
    rm -rf /tmp/output/*
    filename=${filenames[$i]}
    if ./fastar http://localhost:8000/$filename -C /tmp/output; then
        if diff /tmp/output/tmp/rand /tmp/rand; then
            echo $file matches
        else
            echo xxx $file does not match
            ret=1
        fi
    else
        ret=1
    fi
done

echo testing hardcoded compression
for ((i = 0; i < ${#filenames[@]}; ++i))
do
    rm -rf /tmp/output/*
    filename=${filenames[$i]}
    compression=${compressions[$i]}
    if ./fastar http://localhost:8000/$filename -C /tmp/output --compression $compression; then
        if diff /tmp/output/tmp/rand /tmp/rand; then
            echo $file matches
        else
            echo xxx $file does not match
            ret=1
        fi
    else
        ret=1
    fi
done

echo killing fileserver...
kill -9 $pid

exit $ret
