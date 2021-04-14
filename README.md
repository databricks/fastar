# fastar

A faster way to download and extract tarballs.

## Motivation

When downloading a .tar/.tar.gz/.tar.lz4 file from the internet, users rarely leave it at that. They usually want to extract it to get the contents inside.

One method that can help speed this up is to get started on the decompression/extraction of the tarball while still downloading it. 
This can be done by piping the output of a download directly to tar as shown below.

`wget http://foo.net/bar.tar.lz4 -O - | tar -xf - -C /dir/to/extract/to -I lz4`

Here, `-O -` tells wget to pipe the download stream to stdout, which tar takes in by specifying an input file of `-` (read from stdin).
As a best case scenario, if both downloading and extracting the file individually take the same amount of time, this can cut overall time by as much as half.

One problem with the above solution however is that it uses a fairly slow method for downloading the source file.
Commands like wget and curl use a single session with the fileserver to download the file contents in a single stream.

There are other alternatives like [aria2c](https://github.com/aria2/aria2) which support multiple parallel streams for faster downloads, but as far as I could find, none of these also supported piping the data to stdout.
I suspect this is because of the added logic to serialize the parallel chunks into the correct order to stream to stdout.
Thus the primary motivation for this project was to make a file downloader that could take advantage of parallel input streams while still serializing the data for simultaneous extraction by tar.

## How it works
Fastar employs a group of worker threads that are all responsible for their own slices of the overall file to download.
Similar to other parallel downloaders, it takes advantage of the HTTP RANGE header to make sure each worker only downloads its chunk.
The main difference is that these workers make use of channels and a shared io.Writer to synchronize the eventual stream of data that is written.
This allows for multiple workers to be constantly pulling data in parallel even while one is writing its data out to the eventual consumer.

Assuming 4 worker threads (this number is user configurable), the high level logic would be as follows:

1. Kick off threads (T1 - T4) which start downloading chunks in parallel starting at the beginning of the file.
T1 starts immediately writing to stdout while T2-T4 save to an in-memory buffer until it's their turn.
```
+----+----+----+----+-------------------+
|    |    |    |    |                   |
| T1 | T2 | T3 | T4 |                   |
|    |    |    |    |                   |
+----+----+----+----+-------------------+
```

2. Once T1 is finished writing its current chunk to stdout, it signals to T2 that it's their turn, and starts downloading the next chunk it's responsible for (right after T4's current chunk).
   T2 starts writing the data they saved in their buffer to stdout while the rest of the threads continue with their downloads. This process is repeated for the whole file.
```
+----+----+----+----+----+--------------+
|    |    |    |    |    |              |
|    | T2 | T3 | T4 | T1 |              |
|    |    |    |    |    |              |
+----+----+----+----+----+--------------+
```

## Perf numbers
These all use a lz4 compressed tarball of a container filesystem (2.6GB compressed, 4.3GB uncompressed), hosted on a ramFS local fileserver.
Average of 3 runs taken.

Download to /dev/null:
|wget|fastar|
---|---
|1.34s|0.55s|

Download + decompress to /dev/null (`wget | lz4` vs `fastar`):
|wget+lz4|fastar|
---|---
|5.11s|3.45s|

Download + decompress + extract to ramFS (`aria2c && tar` vs `wget | tar` vs `fastar | tar` vs `fastar -C`):
|wget+tar|aria2c+tar|fastar+tar|fastar|
---|---|---|---
|16.78s|13.38s|11.02s|6.41s|
