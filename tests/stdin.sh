#!/bin/bash

ret=0

echo building binaries
go build

rm -rf /tmp/source
rm -rf /tmp/download
dd if=/dev/urandom of=/tmp/source bs=1 count=4k
cat /tmp/source | ./fastar -O > /tmp/download
if diff /tmp/source /tmp/download; then
    echo files match
else
    echo xxx files do not match
    ret=1
fi

exit $ret
