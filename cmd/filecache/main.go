// Copyright 2022 Tamás Gulácsi.

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/UNO-SOFT/filecache"
	"github.com/peterbourgon/ff/v3/ffcli"
)

func main() {
	if err := Main(); err != nil {
		log.Fatalf("ERROR: %+v", err)
	}
}

func Main() error {
	fs := flag.NewFlagSet("filecache", flag.ContinueOnError)
	flagCacheDir := fs.String("cache-dir", "", "cache directory")
	*flagCacheDir, _ = os.UserCacheDir()
	flagStdin := fs.Bool("stdin", false, "read and pass stdin")
	flagTrim := fs.Bool("trim", false, "trim before run")
	flagMTimeInterval := fs.Duration("mtime", filecache.DefaultMTimeInterval, "mtime resolution")
	flagTrimInterval := fs.Duration("trim-interval", filecache.DefaultMTimeInterval, "trim interval")
	flagTrimLimit := fs.Duration("trim-limit", filecache.DefaultTrimLimit, "trim limit")

	app := ffcli.Command{Name: "cmd", FlagSet: fs,
		ShortUsage: "command to execute",
		Exec: func(ctx context.Context, args []string) error {
			cache, err := filecache.Open(*flagCacheDir)
			if err != nil {
				return fmt.Errorf("open %q: %w", *flagCacheDir, err)
			}
			if *flagMTimeInterval > 0 {
				cache.SetMTimeInterval(*flagMTimeInterval)
			}
			if *flagTrim {
				cache.TrimWithLimit(*flagTrimInterval, *flagTrimLimit)
			}
			var cmdBuf bytes.Buffer
			for _, arg := range args {
				_, _ = cmdBuf.WriteString(arg)
				_ = cmdBuf.WriteByte(0)
			}

			var stdin io.Reader
			if *flagStdin {
				hsh := filecache.NewHash()
				fh, err := os.CreateTemp("", "filecache-*.inp")
				if err != nil {
					return err
				}
				defer func() {
					_ = fh.Close()
					_ = os.Remove(fh.Name())
				}()
				if _, err = io.Copy(io.MultiWriter(hsh, fh), os.Stdin); err != nil {
					return fmt.Errorf("copy stdin to temp file %q: %w", fh.Name(), err)
				}
				if _, err = fh.Seek(0, 0); err != nil {
					return fmt.Errorf("rewind %q: %w", fh.Name(), err)
				}
				stdin = fh
				_ = os.Remove(fh.Name())
				_ = cmdBuf.WriteByte(0)
				sumID := hsh.SumID()
				_, _ = cmdBuf.Write(sumID[:])
				_ = cmdBuf.WriteByte(0)
				log.Printf("stdinHash=%x", hsh.SumID())
			}

			actionID := filecache.NewActionID(cmdBuf.Bytes())
			log.Printf("actionID=%x", actionID)
			fn, _, err := cache.GetFile(actionID)
			if fn != "" && err == nil {
				fh, err := os.Open(fn)
				if err != nil {
					log.Printf("open %q: %+v", fn, err)
				} else {
					_, err = io.Copy(os.Stdout, fh)
					return err
				}
			}

			fh, err := os.CreateTemp("", "filecache-*.out")
			if err != nil {
				return err
			}
			defer func() {
				_ = fh.Close()
				_ = os.Remove(fh.Name())
			}()
			cmd := exec.CommandContext(ctx, args[0], args[1:]...)
			if stdin != nil {
				cmd.Stdin = stdin
			}
			cmd.Stderr = os.Stderr
			cmd.Stdout = io.MultiWriter(fh, os.Stdout)
			if err = cmd.Run(); err != nil {
				return err
			}
			_ = os.Remove(fh.Name())
			if _, err = fh.Seek(0, 0); err != nil {
				return fmt.Errorf("rewind %q: %w", fh.Name(), err)
			}
			_, _, err = cache.Put(actionID, fh)
			return err
		},
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return app.ParseAndRun(ctx, os.Args[1:])
}
