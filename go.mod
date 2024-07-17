module github.com/UNO-SOFT/filecache

go 1.21

toolchain go1.21.0

require (
	github.com/UNO-SOFT/zlog v0.7.7
	github.com/google/renameio/v2 v2.0.0
	github.com/peterbourgon/ff/v3 v3.1.2
	github.com/rogpeppe/go-internal v1.12.0
	github.com/tgulacsi/go v0.24.3
)

require (
	github.com/dgryski/go-linebreak v0.0.0-20180812204043-d8f37254e7d3 // indirect
	github.com/go-logr/logr v1.2.4 // indirect
	golang.org/x/exp v0.0.0-20230713183714-613f0c0eb8a1 // indirect
	golang.org/x/sys v0.10.0 // indirect
	golang.org/x/term v0.10.0 // indirect
)

//replace github.com/UNO-SOFT/zlog => ../zlog
