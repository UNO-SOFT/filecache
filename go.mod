module github.com/UNO-SOFT/filecache

go 1.27.0

require (
	github.com/UNO-SOFT/zlog v0.8.3
	github.com/google/renameio/v2 v2.0.0
	github.com/peterbourgon/ff/v4 v4.0.0-beta.1
	github.com/rogpeppe/go-internal v1.16.0
	github.com/tgulacsi/go v0.27.7-0.20241126105246-43f36a11adc5
)

require (
	github.com/UNO-SOFT/cli v0.0.0-20261009074406-95b18dbd4953 // indirect
	github.com/dgryski/go-linebreak v0.0.0-20180812204043-d8f37254e7d3 // indirect
	github.com/go-logr/logr v1.4.2 // indirect
	golang.org/x/exp v0.0.0-20241217172543-b2144cdd0a67 // indirect
	golang.org/x/sys v0.28.0 // indirect
	golang.org/x/term v0.27.0 // indirect
)

//replace github.com/UNO-SOFT/zlog => ../zlog

replace github.com/peterbourgon/ff/v4 v4.0.0-beta.1 => github.com/UNO-SOFT/ff/v4 v4.0.0-beta.1.us
