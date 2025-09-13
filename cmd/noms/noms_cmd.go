// Copyright 2016 Attic Labs, Inc. All rights reserved.
// Licensed under the Apache License, version 2.0:
// http://www.apache.org/licenses/LICENSE-2.0

package main

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"

	"github.com/attic-labs/kingpin"
	"github.com/attic-labs/noms/cmd/util"
	"github.com/attic-labs/noms/go/config"
	"github.com/attic-labs/noms/go/constants"
	"github.com/attic-labs/noms/go/d"
	"github.com/attic-labs/noms/go/datas"
	"github.com/attic-labs/noms/go/spec"
	"github.com/attic-labs/noms/go/types"
	"github.com/attic-labs/noms/go/util/datetime"
	"github.com/attic-labs/noms/go/util/outputpager"

	"github.com/chzyer/readline"
)

func try(f func() int) (v int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("error: %v\n", r)
			v = 0 // continue
		}
	}()
	return f()
}

func nomsCmd(noms *kingpin.Application) (*kingpin.CmdClause, util.KingpinHandler) {
	cmd := noms.Command("cmd", "Interactively work with Noms data.")
	dbName := cmd.Arg("name", "name of the database to operate - see Spelling Objects at https://github.com/attic-labs/noms/blob/master/doc/spelling.md").String()

	return cmd, func(input string) int {
		cfg := config.NewResolver()
		db, err := cfg.GetDatabase(*dbName)
		d.CheckError(err)
		rl, err := readline.New("noms> ")
		if err != nil {
			panic(err)
		}
		for {
			r := try(func() int {
				var args []string
				var line string
				line, err = readFromRL(rl)
				if err != nil { // io.EOF
					return 1
				}
				line = strings.TrimSpace(line)
				if line == "" {
					return 0
				}
				args = parseArgs(line)
				command := args[0]
				if command == "exit" || command == "quit" {
					return 1
				}
				
				db.Rebase()

				if command == "version" {
					fmt.Printf("format version: %v\n", constants.NomsVersion)
					fmt.Printf("built from %v\n", constants.NomsGitSHA)
				} else if command == "ds" {
					if len(args) > 1 {
						exp := args[1]
						re, err := regexp.Compile(exp)
						d.CheckError(err)
						db.Datasets().IterAll(func(k, v types.Value) {
							if re.MatchString(string(k.(types.String))) {
								fmt.Println(k)
							}
						})
						return 0
					}
					db.Datasets().IterAll(func(k, v types.Value) {
						fmt.Println(k)
					})
				} else if command == "show" {
					path, err := spec.NewAbsolutePath(args[1])
					if err != nil {
						fmt.Printf("error: %v\n", err)
						return 0
					}
					v := path.Resolve(db)
					if v == nil {
						fmt.Printf("<nil>\n")
						return 0
					}

					func() {
						pgr := outputpager.Start()
						defer pgr.Stop()

						err := types.WriteEncodedValue(pgr.Writer, v)
						if err != nil {
							fmt.Printf("error: %v\n", err)
							return
						}
						fmt.Fprintln(pgr.Writer)
					}()
				} else if command == "log" {
					o := opts{}
					o.useColor = true
					o.tz, _ = locationFromTimezoneArg("local", nil)
					o.path = args[1]
					o.oneline = true
					o.showGraph = true
					if len(args) > 2 && args[2] == "v" {
						o.oneline = false
						o.showGraph = false
					}
					err := nomsCmdLog(db, o)
					if err != nil {
						fmt.Printf("error: %v\n", err)
					}
				} else {
					fmt.Printf("Unrecognized command: %s\n", command)
				}
				return 0
			})

			if r != 0 {
				break
			}
		}
		return 0
	}
}

func readFromRL(rl *readline.Instance) (string, error) {
	defer rl.SetPrompt("noms> ")
	quoteCount := 0
	res := ""
	for {
		line, err := rl.Readline()
		if err != nil {
			return "", err
		}
		for _, c := range line {
			if c == '"' {
				quoteCount++
			}
		}
		res += line + "\n"
		if quoteCount % 2 == 0 {
			return res, nil
		}
		rl.SetPrompt("... ")
	}
}

func nomsCmdLog(db datas.Database, o opts) error {
	datetime.RegisterHRSCommenter(o.tz)

	ds  := db.GetDataset(o.path)
	path := types.MustParsePath(".value")

	origCommit, ok := ds.MaybeHead()
	if !ok || !datas.IsCommit(origCommit) {
		return fmt.Errorf("%v is not a Commit object", origCommit)
	}

	iter := NewCommitIterator(db, origCommit)
	displayed := 0
	if o.maxCommits <= 0 {
		o.maxCommits = math.MaxInt32
	}

	bytesChan := make(chan chan []byte, parallelism)

	var done = false

	go func() {
		for ln, ok := iter.Next(); !done && ok && displayed < o.maxCommits; ln, ok = iter.Next() {
			ch := make(chan []byte)
			bytesChan <- ch

			go func(ch chan []byte, node LogNode) {
				buff := &bytes.Buffer{}
				printCommit(node, path, buff, db, o)
				ch <- buff.Bytes()
			}(ch, ln)

			displayed++
		}
		close(bytesChan)
	}()

	pgr := outputpager.Start()
	defer pgr.Stop()

	for ch := range bytesChan {
		commitBuff := <-ch
		_, err := io.Copy(pgr.Writer, bytes.NewReader(commitBuff))
		if err != nil {
			done = true
			for range bytesChan {
				// drain the output
			}
		}
	}
	return nil
}

func parseArgs(line string) []string {
	return strings.Fields(line)
}
