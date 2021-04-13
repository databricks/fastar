#!/bin/bash

set -e

ret=0

echo building binaries
go build
cd fileserver
go build ./fileserver.go
cd ..

echo copying test tarball to /tmp
/bin/cp image.tar.lz4 /tmp/image.tar.lz4

echo starting fileserver
./fileserver/fileserver &
pid=$!
echo fileserver pid is $pid, saving for later

echo making dest directories
rm -rf ~/tar
rm -rf ~/fastar
mkdir -p ~/tar
mkdir -p ~/fastar

echo running control command
wget -q http://localhost:8000/image.tar.lz4 -O - | lz4 -dc | tar -xf - -C ~/tar

echo running fastar command
./fastar http://localhost:8000/image.tar.lz4 -C ~/fastar

echo checking if results differ
if diff --no-dereference -bur ~/tar ~/fastar; then
  echo directories match
else
  echo directories do not match
  ret=1
fi

echo killing fileserver...
kill -9 $pid

exit $ret
