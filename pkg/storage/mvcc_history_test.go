// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package storage_test

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/concurrency/lock"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/spanset"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/uncertainty"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/storage/enginepb"
	"github.com/cockroachdb/cockroach/pkg/testutils/datapathutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/skip"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/uint128"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/cockroachdb/datadriven"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/sstable"
	"github.com/cockroachdb/redact"
	"github.com/stretchr/testify/require"
)

var (
	clearRangeUsingIter = util.ConstantWithMetamorphicTestBool(
		"mvcc-histories-clear-range-using-iterator", false)
	cmdDeleteRangeTombstoneKnownStats = util.ConstantWithMetamorphicTestBool(
		"mvcc-histories-deleterange-tombstome-known-stats", false)
	mvccHistoriesReader = util.ConstantWithMetamorphicTestChoice("mvcc-histories-reader",
		"engine", "readonly", "batch", "snapshot").(string)
	mvccHistoriesUseBatch   = util.ConstantWithMetamorphicTestBool("mvcc-histories-use-batch", false)
	mvccHistoriesPeekBounds = util.ConstantWithMetamorphicTestChoice("mvcc-histories-peek-bounds",
		"none", "left", "right", "both").(string)
	sstIterVerify           = util.ConstantWithMetamorphicTestBool("mvcc-histories-sst-iter-verify", false)
	metamorphicIteratorSeed = util.ConstantWithMetamorphicTestRange("mvcc-metamorphic-iterator-seed", 0, 0, 100000) // 0 = disabled
	separateEngineBlocks    = util.ConstantWithMetamorphicTestBool("mvcc-histories-separate-engine-blocks", false)
)

// TestMVCCHistories verifies that sequences of MVCC reads and writes
// perform properly.
//
// The input files use the following DSL:
//
// run            [ok|trace|stats|error]
//
// txn_begin      t=<name> [ts=<int>[,<int>]] [globalUncertaintyLimit=<int>[,<int>]]
// txn_remove     t=<name>
// txn_restart    t=<name>
// txn_update     t=<name> t2=<name>
// txn_step       t=<name> [n=<int>]
// txn_advance    t=<name> ts=<int>[,<int>]
// txn_status     t=<name> status=<txnstatus>
// txn_ignore_seqs t=<name> seqs=[<int>-<int>[,<int>-<int>...]]
//
// resolve_intent t=<name> k=<key> [status=<txnstatus>] [clockWhilePending=<int>[,<int>]] [targetBytes=<int>]
// resolve_intent_range t=<name> k=<key> end=<key> [status=<txnstatus>] [maxKeys=<int>] [targetBytes=<int>]
// check_intent   k=<key> [none]
// add_lock       t=<name> k=<key>
//
// cput           [t=<name>] [ts=<int>[,<int>]] [localTs=<int>[,<int>]] [resolve [status=<txnstatus>]] k=<key> v=<string> [raw] [cond=<string>]
// del            [t=<name>] [ts=<int>[,<int>]] [localTs=<int>[,<int>]] [resolve [status=<txnstatus>]] k=<key>
// del_range      [t=<name>] [ts=<int>[,<int>]] [localTs=<int>[,<int>]] [resolve [status=<txnstatus>]] k=<key> [end=<key>] [max=<max>] [returnKeys]
// del_range_ts   [ts=<int>[,<int>]] [localTs=<int>[,<int>]] k=<key> end=<key> [idempotent] [noCoveredStats]
// del_range_pred [ts=<int>[,<int>]] [localTs=<int>[,<int>]] k=<key> end=<key> [startTime=<int>,max=<int>,maxBytes=<int>,rangeThreshold=<int>]
// increment      [t=<name>] [ts=<int>[,<int>]] [localTs=<int>[,<int>]] [resolve [status=<txnstatus>]] k=<key> [inc=<val>]
// initput        [t=<name>] [ts=<int>[,<int>]] [resolve [status=<txnstatus>]] k=<key> v=<string> [raw] [failOnTombstones]
// merge          [t=<name>] [ts=<int>[,<int>]] [resolve [status=<txnstatus>]] k=<key> v=<string> [raw]
// put            [t=<name>] [ts=<int>[,<int>]] [localTs=<int>[,<int>]] [resolve [status=<txnstatus>]] k=<key> v=<string> [raw]
// put_rangekey   ts=<int>[,<int>] [localTs=<int>[,<int>]] k=<key> end=<key>
// put_blind_inline	k=<key> v=<string> [prev=<string>]
// get            [t=<name>] [ts=<int>[,<int>]]                         [resolve [status=<txnstatus>]] k=<key> [inconsistent] [skipLocked] [tombstones] [failOnMoreRecent] [localUncertaintyLimit=<int>[,<int>]] [globalUncertaintyLimit=<int>[,<int>]] [maxKeys=<int>] [targetBytes=<int>] [allowEmpty]
// scan           [t=<name>] [ts=<int>[,<int>]]                         [resolve [status=<txnstatus>]] k=<key> [end=<key>] [inconsistent] [skipLocked] [tombstones] [reverse] [failOnMoreRecent] [localUncertaintyLimit=<int>[,<int>]] [globalUncertaintyLimit=<int>[,<int>]] [max=<max>] [targetbytes=<target>] [wholeRows[=<int>]] [allowEmpty]
// export         [k=<key>] [end=<key>] [ts=<int>[,<int>]] [kTs=<int>[,<int>]] [startTs=<int>[,<int>]] [maxIntents=<int>] [allRevisions] [targetSize=<int>] [maxSize=<int>] [stopMidKey] [fingerprint]
//
// iter_new       [k=<key>] [end=<key>] [prefix] [kind=key|keyAndIntents] [types=pointsOnly|pointsWithRanges|pointsAndRanges|rangesOnly] [maskBelow=<int>[,<int>]]
// iter_new_incremental [k=<key>] [end=<key>] [startTs=<int>[,<int>]] [endTs=<int>[,<int>]] [types=pointsOnly|pointsWithRanges|pointsAndRanges|rangesOnly] [maskBelow=<int>[,<int>]] [intents=error|aggregate|emit]
// iter_seek_ge   k=<key> [ts=<int>[,<int>]]
// iter_seek_lt   k=<key> [ts=<int>[,<int>]]
// iter_seek_intent_ge k=<key> txn=<name>
// iter_next
// iter_next_ignoring_time
// iter_next_key_ignoring_time
// iter_next_key
// iter_prev
// iter_scan      [reverse]
//
// merge     [ts=<int>[,<int>]] k=<key> v=<string> [raw]
//
// clear				  k=<key> [ts=<int>[,<int>]]
// clear_range    k=<key> end=<key>
// clear_rangekey k=<key> end=<key> ts=<int>[,<int>]
// clear_time_range k=<key> end=<key> ts=<int>[,<int>] targetTs=<int>[,<int>] [clearRangeThreshold=<int>] [maxBatchSize=<int>] [maxBatchByteSize=<int>]
//
// gc_clear_range k=<key> end=<key> startTs=<int>[,<int>] ts=<int>[,<int>]
// gc_points_clear_range k=<key> end=<key> startTs=<int>[,<int>] ts=<int>[,<int>]
// replace_point_tombstones_with_range_tombstones k=<key> [end=<key>]
//
// sst_put            [ts=<int>[,<int>]] [localTs=<int>[,<int>]] k=<key> [v=<string>]
// sst_put_rangekey   ts=<int>[,<int>] [localTS=<int>[,<int>]] k=<key> end=<key>
// sst_clear_range    k=<key> end=<key>
// sst_clear_rangekey k=<key> end=<key> ts=<int>[,<int>]
// sst_finish
// sst_iter_new
//
// Where `<key>` can be a simple string, or a string
// prefixed by the following characters:
//
// - `=foo` means exactly key `foo`
// - `+foo` means `Key(foo).Next()`
// - `-foo` means `Key(foo).PrefixEnd()`
// - `%foo` means `append(LocalRangePrefix, "foo")`
// - `/foo/7` means SQL row with key foo, optional column family 7 (system tenant, table/index 1).
//
// Additionally, the pseudo-command `with` enables sharing
// a group of arguments between multiple commands, for example:
//
//	with t=A
//	  txn_begin
//	  with k=a
//	    put v=b
//	    resolve_intent
//
// Really means:
//
//	txn_begin          t=A
//	put v=b        k=a t=A
//	resolve_intent k=a t=A
func TestMVCCHistories(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	// TODO(storage-team): this prevents us from easily finding bugs which
	// incorrectly assume simple value encoding. We only find bugs where we are
	// explicitly using the extended encoding by setting a localTs. One way to
	// handle the different test output with extended value encoding would be to
	// duplicate each test file for the two cases.
	storage.DisableMetamorphicSimpleValueEncoding(t)

	ctx := context.Background()

	// intentInterleavingIter doesn't allow iterating from the local to the global
	// keyspace, so we have to process these key spans separately.
	spans := []roachpb.Span{
		{Key: keys.MinKey, EndKey: roachpb.LocalMax},
		{Key: keys.LocalMax, EndKey: roachpb.KeyMax},
	}

	// Timestamp for MVCC stats calculations, in nanoseconds.
	const statsTS = 100e9

	datadriven.Walk(t, datapathutils.TestDataPath(t, "mvcc_histories"), func(t *testing.T, path string) {
		st := cluster.MakeTestingClusterSettings()

		if strings.Contains(path, "_norace") {
			skip.UnderRace(t)
		}

		if strings.Contains(path, "_disable_local_timestamps") {
			storage.LocalTimestampsEnabled.Override(ctx, &st.SV, false)
		}

		disableSeparateEngineBlocks := strings.Contains(path, "_disable_separate_engine_blocks")

		// We start from a clean slate in every test file.
		engine, err := storage.Open(ctx, storage.InMemory(), st,
			storage.CacheSize(1<<20 /* 1 MiB */),
			storage.If(separateEngineBlocks && !disableSeparateEngineBlocks, storage.BlockSize(1)),
		)
		require.NoError(t, err)
		defer engine.Close()

		reportDataEntries := func(buf *redact.StringBuilder) error {
			var hasData bool

			for _, span := range spans {
				err = engine.MVCCIterate(
					span.Key, span.EndKey, storage.MVCCKeyAndIntentsIterKind, storage.IterKeyTypeRangesOnly,
					func(_ storage.MVCCKeyValue, rangeKeys storage.MVCCRangeKeyStack) error {
						hasData = true
						buf.Printf("rangekey: %s/[", rangeKeys.Bounds)
						for i, version := range rangeKeys.Versions {
							val, err := storage.DecodeMVCCValue(version.Value)
							require.NoError(t, err)
							if i > 0 {
								buf.Printf(" ")
							}
							buf.Printf("%s=%s", version.Timestamp, val)
						}
						buf.Printf("]\n")
						return nil
					})
				if err != nil {
					return err
				}

				err = engine.MVCCIterate(
					span.Key, span.EndKey, storage.MVCCKeyAndIntentsIterKind, storage.IterKeyTypePointsOnly,
					func(r storage.MVCCKeyValue, _ storage.MVCCRangeKeyStack) error {
						hasData = true
						if r.Key.Timestamp.IsEmpty() {
							// Meta is at timestamp zero.
							meta := enginepb.MVCCMetadata{}
							if err := protoutil.Unmarshal(r.Value, &meta); err != nil {
								buf.Printf("meta: %v -> error decoding proto from %v: %v\n", r.Key, r.Value, err)
							} else {
								buf.Printf("meta: %v -> %+v\n", r.Key, &meta)
							}
						} else {
							val, err := storage.DecodeMVCCValue(r.Value)
							if err != nil {
								buf.Printf("data: %v -> error decoding value %v: %v\n", r.Key, r.Value, err)
							} else {
								buf.Printf("data: %v -> %s\n", r.Key, val)
							}
						}
						return nil
					})
			}

			if !hasData {
				buf.SafeString("<no data>\n")
			}
			return err
		}

		// reportSSTEntries outputs entries from a raw SSTable. It uses a raw
		// SST iterator in order to accurately represent the raw SST data.
		reportSSTEntries := func(buf *redact.StringBuilder, name string, sst []byte) error {
			r, err := sstable.NewMemReader(sst, sstable.ReaderOptions{
				Comparer: storage.EngineComparer,
			})
			if err != nil {
				return err
			}
			buf.Printf(">> %s:\n", name)

			// Dump point keys.
			iter, err := r.NewIter(nil, nil)
			if err != nil {
				return err
			}
			defer func() { _ = iter.Close() }()
			for k, lv := iter.SeekGE(nil, sstable.SeekGEFlags(0)); k != nil; k, lv = iter.Next() {
				if err := iter.Error(); err != nil {
					return err
				}
				key, err := storage.DecodeMVCCKey(k.UserKey)
				if err != nil {
					return err
				}
				v, _, err := lv.Value(nil)
				if err != nil {
					return err
				}
				value, err := storage.DecodeMVCCValue(v)
				if err != nil {
					return err
				}
				buf.Printf("%s: %s -> %s\n", strings.ToLower(k.Kind().String()), key, value)
			}

			// Dump rangedels.
			if rdIter, err := r.NewRawRangeDelIter(); err != nil {
				return err
			} else if rdIter != nil {
				defer func() { _ = rdIter.Close() }()
				for s := rdIter.SeekGE(nil); s != nil; s = rdIter.Next() {
					if err := rdIter.Error(); err != nil {
						return err
					}
					start, err := storage.DecodeMVCCKey(s.Start)
					if err != nil {
						return err
					}
					end, err := storage.DecodeMVCCKey(s.End)
					if err != nil {
						return err
					}
					for _, k := range s.Keys {
						buf.Printf("%s: %s\n", strings.ToLower(k.Kind().String()),
							roachpb.Span{Key: start.Key, EndKey: end.Key})
					}
				}
			}

			// Dump range keys.
			if rkIter, err := r.NewRawRangeKeyIter(); err != nil {
				return err
			} else if rkIter != nil {
				defer func() { _ = rkIter.Close() }()
				for s := rkIter.SeekGE(nil); s != nil; s = rkIter.Next() {
					if err := rkIter.Error(); err != nil {
						return err
					}
					start, err := storage.DecodeMVCCKey(s.Start)
					if err != nil {
						return err
					}
					end, err := storage.DecodeMVCCKey(s.End)
					if err != nil {
						return err
					}
					for _, k := range s.Keys {
						buf.Printf("%s: %s", strings.ToLower(k.Kind().String()),
							roachpb.Span{Key: start.Key, EndKey: end.Key})
						if k.Suffix != nil {
							ts, err := storage.DecodeMVCCTimestampSuffix(k.Suffix)
							if err != nil {
								return err
							}
							buf.Printf("/%s", ts)
						}
						if k.Kind() == pebble.InternalKeyKindRangeKeySet {
							value, err := storage.DecodeMVCCValue(k.Value)
							if err != nil {
								return err
							}
							buf.Printf(" -> %s", value)
						}
						buf.Printf("\n")
					}
				}
			}

			return nil
		}

		e := newEvalCtx(ctx, engine)
		defer e.close()
		if strings.Contains(path, "_nometamorphiciter") {
			e.noMetamorphicIter = true
		}

		datadriven.RunTest(t, path, func(t *testing.T, d *datadriven.TestData) string {
			// We'll be overriding cmd/cmdargs below, because the
			// datadriven reader does not know about sub-commands.
			defer func(pos, cmd string, cmdArgs []datadriven.CmdArg) {
				d.Pos = pos
				d.Cmd = cmd
				d.CmdArgs = cmdArgs
			}(d.Pos, d.Cmd, d.CmdArgs)
			// The various evalCtx helpers want access to the current test
			// and testdata structs.
			e.t = t
			e.td = d

			switch d.Cmd {
			case "skip":
				if len(d.CmdArgs) == 0 {
					skip.IgnoreLint(e.t, "skipped")
				}
				return d.Expected
			case "run":
				// Syntax: run [trace] [error]
				// (other words - in particular "ok" - are accepted but ignored)
				//
				// "run" executes a script of zero or more operations from
				// the commands library defined below.
				// It stops upon the first error encountered, if any.
				//
				// Options:
				// - trace: emit intermediate results after each operation.
				// - stats: emit MVCC statistics for each operation and at the end.
				// - error: expect an error to occur. The specific error type/ message
				//   to expect is spelled out in the expected output.
				//
				trace := e.hasArg("trace")
				stats := e.hasArg("stats")
				expectError := e.hasArg("error")

				// buf will accumulate the actual output, which the
				// datadriven driver will use to compare to the expected
				// output.
				var buf redact.StringBuilder
				e.results.buf = &buf
				e.results.traceIntentWrites = trace

				// We reset the stats such that they accumulate for all commands
				// in a single test.
				e.ms = &enginepb.MVCCStats{}

				// foundErr remembers which error was last encountered while
				// executing the script under "run".
				var foundErr error

				// pos is the original <file>:<lineno> prefix computed by
				// datadriven. It points to the top "run" command itself.
				// We editing d.Pos in-place below by extending `pos` upon
				// each new line of the script.
				pos := d.Pos

				// dataChange indicates whether some command in the script
				// has modified the stored data. When this becomes true, the
				// current content of storage is printed in the results
				// buffer at the end.
				dataChange := false
				// txnChange indicates whether some command has modified
				// a transaction object. When set, the last modified txn
				// object is reported in the result buffer at the end.
				txnChange := false
				// locksChange indicates whether some command has modified
				// the lock table. When set, the lock table is reported in
				// the result buffer at the end.
				locksChange := false

				reportResults := func(printTxn, printData, printLocks bool) {
					if printTxn && e.results.txn != nil {
						buf.Printf("txn: %v\n", e.results.txn)
					}
					if printData {
						err := reportDataEntries(&buf)
						if err != nil {
							if foundErr == nil {
								// Handle the error below.
								foundErr = err
							} else {
								buf.Printf("error reading data: (%T:) %v\n", err, err)
							}
						}
						for i, sst := range e.ssts {
							err = reportSSTEntries(&buf, fmt.Sprintf("sst-%d", i), sst)
							if err != nil {
								if foundErr == nil {
									// Handle the error below.
									foundErr = err
								} else {
									buf.Printf("error reading SST data: (%T:) %v\n", err, err)
								}
							}
						}
					}
					if printLocks {
						var ks []string
						for k := range e.locks {
							ks = append(ks, k)
						}
						sort.Strings(ks)
						buf.Printf("lock-table: {")
						for i, k := range ks {
							if i > 0 {
								buf.Printf(", ")
							}
							buf.Printf("%s:%s", k, e.locks[k].ID)
						}
						buf.Printf("}\n")
					}
				}

				// sharedCmdArgs is updated by "with" pseudo-commands,
				// to pre-populate common arguments for the following
				// indented commands.
				var sharedCmdArgs []datadriven.CmdArg

				// The lines of the script under "run".
				lines := strings.Split(d.Input, "\n")
				for i, line := range lines {
					if short := strings.TrimSpace(line); short == "" || strings.HasPrefix(short, "#") {
						// Comment or empty line. Do nothing.
						continue
					}

					// Compute a line prefix, to clarify error message. We
					// prefix a newline character because some text editor do
					// not know how to jump to the location of an error if
					// there are multiple file:line prefixes on the same line.
					d.Pos = fmt.Sprintf("\n%s: (+%d)", pos, i+1)

					// Trace the execution in testing.T, to clarify where we
					// are in case an error occurs.
					log.Infof(context.Background(), "TestMVCCHistories:\n\t%s: %s", d.Pos, line)

					// Decompose the current script line.
					var err error
					d.Cmd, d.CmdArgs, err = datadriven.ParseLine(line)
					if err != nil {
						e.t.Fatalf("%s: %v", d.Pos, err)
					}

					// Expand "with" commands:
					//   with t=A
					//       txn_begin
					//       resolve_intent k=a
					// is equivalent to:
					//   txn_begin      t=A
					//   resolve_intent k=a t=A
					isIndented := strings.TrimLeft(line, " \t") != line
					if d.Cmd == "with" {
						if !isIndented {
							// Reset shared args.
							sharedCmdArgs = d.CmdArgs
						} else {
							// Prefix shared args. We use prefix so that the
							// innermost "with" can override/shadow the outermost
							// "with".
							sharedCmdArgs = append(d.CmdArgs, sharedCmdArgs...)
						}
						continue
					} else if isIndented {
						// line is indented. Inherit arguments.
						if len(sharedCmdArgs) == 0 {
							// sanity check.
							e.Fatalf("indented command without prior 'with': %s", line)
						}
						// We prepend the args that are provided on the command
						// itself so it's possible to override those provided
						// via "with".
						d.CmdArgs = append(d.CmdArgs, sharedCmdArgs...)
					} else {
						// line is not indented. Clear shared arguments.
						sharedCmdArgs = nil
					}

					cmd := e.getCmd()
					txnChange = txnChange || cmd.typ == typTxnUpdate
					dataChange = dataChange || cmd.typ == typDataUpdate
					locksChange = locksChange || cmd.typ == typLocksUpdate

					if trace || (stats && cmd.typ == typDataUpdate) {
						// If tracing is also requested by the datadriven input,
						// we'll trace the statement in the actual results too.
						buf.Printf(">> %s", d.Cmd)
						for i := range d.CmdArgs {
							buf.Printf(" %s", &d.CmdArgs[i])
						}
						_ = buf.WriteByte('\n')
					}

					// Record the engine and evaluated stats before the command, so
					// that we can compare the deltas.
					var msEngineBefore enginepb.MVCCStats
					if stats {
						for _, span := range spans {
							ms, err := storage.ComputeStats(e.engine, span.Key, span.EndKey, statsTS)
							require.NoError(t, err)
							msEngineBefore.Add(ms)
						}
					}
					msEvalBefore := *e.ms

					// Run the command.
					foundErr = cmd.fn(e)

					if separateEngineBlocks && !disableSeparateEngineBlocks && dataChange {
						require.NoError(t, e.engine.Flush())
					}

					if trace {
						// If tracing is enabled, we report the intermediate results
						// after each individual step in the script.
						// This may modify foundErr too.
						reportResults(cmd.typ == typTxnUpdate, cmd.typ == typDataUpdate, cmd.typ == typLocksUpdate)
					}

					if stats && cmd.typ == typDataUpdate {
						// If stats are enabled, emit evaluated stats returned by the
						// command, and compare them with the real computed stats diff.
						var msEngineDiff enginepb.MVCCStats
						for _, span := range spans {
							ms, err := storage.ComputeStats(e.engine, span.Key, span.EndKey, statsTS)
							require.NoError(t, err)
							msEngineDiff.Add(ms)
						}
						msEngineDiff.Subtract(msEngineBefore)

						msEvalDiff := *e.ms
						msEvalDiff.Subtract(msEvalBefore)
						msEvalDiff.AgeTo(msEngineDiff.LastUpdateNanos)
						buf.Printf("stats: %s\n", formatStats(msEvalDiff, true))

						if msEvalDiff != msEngineDiff {
							e.t.Errorf("MVCC stats mismatch for %q at %s\nReturned: %s\nExpected: %s",
								d.Cmd, d.Pos, formatStats(msEvalDiff, true), formatStats(msEngineDiff, true))
						}
					}

					if foundErr != nil {
						// An error occurred. Stop the script prematurely.
						break
					}
				}
				// End of script.

				// Check for any deferred iterator errors.
				if foundErr == nil {
					foundErr = e.iterErr()
				}

				// Flush any unfinished SSTs.
				if foundErr == nil {
					foundErr = e.finishSST()
				} else {
					e.closeSST()
				}

				if !trace {
					// If we were not tracing, no results were printed yet. Do it now.
					if txnChange || dataChange || locksChange {
						buf.SafeString(">> at end:\n")
					}
					reportResults(txnChange, dataChange, locksChange)
				}

				// Calculate and output final stats if requested and the data changed.
				if stats && dataChange {
					var msFinal enginepb.MVCCStats
					for _, span := range spans {
						ms, err := storage.ComputeStats(e.engine, span.Key, span.EndKey, statsTS)
						require.NoError(t, err)
						msFinal.Add(ms)
					}
					buf.Printf("stats: %s\n", formatStats(msFinal, false))
				}

				signalError := e.t.Errorf
				if txnChange || dataChange || locksChange {
					// We can't recover from an error and continue
					// to proceed further tests, because the state
					// may have changed from what the test may be expecting.
					signalError = e.t.Fatalf
				}

				// Check for errors.
				if foundErr == nil && expectError {
					signalError("%s: expected error, got success", d.Pos)
					return d.Expected
				} else if foundErr != nil {
					if expectError {
						buf.Printf("error: (%T:) %v\n", foundErr, foundErr)
					} else /* !expectError */ {
						signalError("%s: expected success, found: (%T:) %v", d.Pos, foundErr, foundErr)
						return d.Expected
					}
				}

				// We're done. Report the actual results and errors to the
				// datadriven executor.
				return buf.String()

			default:
				e.t.Errorf("%s: unknown command: %s", d.Pos, d.Cmd)
				return d.Expected
			}
		})
	})
}

// getCmd retrieves the cmd entry for the current script line.
func (e *evalCtx) getCmd() cmd {
	e.t.Helper()
	c, ok := commands[e.td.Cmd]
	if !ok {
		e.Fatalf("unknown command: %s", e.td.Cmd)
	}
	return c
}

// cmd represents one supported script command.
type cmd struct {
	typ cmdType
	fn  func(e *evalCtx) error
}

type cmdType int

const (
	typReadOnly cmdType = iota
	typTxnUpdate
	typDataUpdate
	typLocksUpdate
)

// commands is the list of all supported script commands.
var commands = map[string]cmd{
	"txn_advance":     {typTxnUpdate, cmdTxnAdvance},
	"txn_begin":       {typTxnUpdate, cmdTxnBegin},
	"txn_ignore_seqs": {typTxnUpdate, cmdTxnIgnoreSeqs},
	"txn_remove":      {typTxnUpdate, cmdTxnRemove},
	"txn_restart":     {typTxnUpdate, cmdTxnRestart},
	"txn_status":      {typTxnUpdate, cmdTxnSetStatus},
	"txn_step":        {typTxnUpdate, cmdTxnStep},
	"txn_update":      {typTxnUpdate, cmdTxnUpdate},

	"resolve_intent":       {typDataUpdate, cmdResolveIntent},
	"resolve_intent_range": {typDataUpdate, cmdResolveIntentRange},
	"check_intent":         {typReadOnly, cmdCheckIntent},
	"add_lock":             {typLocksUpdate, cmdAddLock},

	"clear":                 {typDataUpdate, cmdClear},
	"clear_range":           {typDataUpdate, cmdClearRange},
	"clear_rangekey":        {typDataUpdate, cmdClearRangeKey},
	"clear_time_range":      {typDataUpdate, cmdClearTimeRange},
	"cput":                  {typDataUpdate, cmdCPut},
	"del":                   {typDataUpdate, cmdDelete},
	"del_range":             {typDataUpdate, cmdDeleteRange},
	"del_range_ts":          {typDataUpdate, cmdDeleteRangeTombstone},
	"del_range_pred":        {typDataUpdate, cmdDeleteRangePredicate},
	"export":                {typReadOnly, cmdExport},
	"get":                   {typReadOnly, cmdGet},
	"gc_clear_range":        {typDataUpdate, cmdGCClearRange},
	"gc_points_clear_range": {typDataUpdate, cmdGCPointsClearRange},
	"increment":             {typDataUpdate, cmdIncrement},
	"initput":               {typDataUpdate, cmdInitPut},
	"merge":                 {typDataUpdate, cmdMerge},
	"put":                   {typDataUpdate, cmdPut},
	"put_blind_inline":      {typDataUpdate, cmdPutBlindInline},
	"put_rangekey":          {typDataUpdate, cmdPutRangeKey},
	"scan":                  {typReadOnly, cmdScan},
	"is_span_empty":         {typReadOnly, cmdIsSpanEmpty},

	"iter_new":                    {typReadOnly, cmdIterNew},
	"iter_new_incremental":        {typReadOnly, cmdIterNewIncremental}, // MVCCIncrementalIterator
	"iter_new_read_as_of":         {typReadOnly, cmdIterNewReadAsOf},    // readAsOfIterator
	"iter_seek_ge":                {typReadOnly, cmdIterSeekGE},
	"iter_seek_lt":                {typReadOnly, cmdIterSeekLT},
	"iter_seek_intent_ge":         {typReadOnly, cmdIterSeekIntentGE},
	"iter_next":                   {typReadOnly, cmdIterNext},
	"iter_next_ignoring_time":     {typReadOnly, cmdIterNextIgnoringTime},    // MVCCIncrementalIterator
	"iter_next_key_ignoring_time": {typReadOnly, cmdIterNextKeyIgnoringTime}, // MVCCIncrementalIterator
	"iter_next_key":               {typReadOnly, cmdIterNextKey},
	"iter_prev":                   {typReadOnly, cmdIterPrev},
	"iter_scan":                   {typReadOnly, cmdIterScan},

	"sst_put":            {typDataUpdate, cmdSSTPut},
	"sst_put_rangekey":   {typDataUpdate, cmdSSTPutRangeKey},
	"sst_clear_range":    {typDataUpdate, cmdSSTClearRange},
	"sst_clear_rangekey": {typDataUpdate, cmdSSTClearRangeKey},
	"sst_finish":         {typDataUpdate, cmdSSTFinish},
	"sst_reset":          {typDataUpdate, cmdSSTReset},
	"sst_iter_new":       {typReadOnly, cmdSSTIterNew},

	"replace_point_tombstones_with_range_tombstones": {typDataUpdate, cmdReplacePointTombstonesWithRangeTombstones},
}

func cmdTxnAdvance(e *evalCtx) error {
	txn := e.getTxn(mandatory)
	ts := e.getTs(txn)
	if ts.Less(txn.ReadTimestamp) {
		e.Fatalf("cannot advance txn to earlier (%s) than its ReadTimestamp (%s)",
			ts, txn.ReadTimestamp)
	}
	txn.WriteTimestamp = ts
	e.results.txn = txn
	return nil
}

func cmdTxnBegin(e *evalCtx) error {
	var txnName string
	e.scanArg("t", &txnName)
	ts := e.getTs(nil)
	globalUncertaintyLimit := e.getTsWithName("globalUncertaintyLimit")
	key := roachpb.KeyMin
	if e.hasArg("k") {
		key = e.getKey()
	}
	txn, err := e.newTxn(txnName, ts, globalUncertaintyLimit, key)
	e.results.txn = txn
	return err
}

func cmdTxnIgnoreSeqs(e *evalCtx) error {
	txn := e.getTxn(mandatory)
	seql := e.getList("seqs")
	is := []enginepb.IgnoredSeqNumRange{}
	for _, s := range seql {
		parts := strings.Split(s, "-")
		if len(parts) != 2 {
			e.Fatalf("syntax error: expected 'a-b', got: '%s'", s)
		}
		a, err := strconv.ParseInt(parts[0], 10, 32)
		if err != nil {
			e.Fatalf("%v", err)
		}
		b, err := strconv.ParseInt(parts[1], 10, 32)
		if err != nil {
			e.Fatalf("%v", err)
		}
		is = append(is, enginepb.IgnoredSeqNumRange{Start: enginepb.TxnSeq(a), End: enginepb.TxnSeq(b)})
	}
	txn.IgnoredSeqNums = is
	e.results.txn = txn
	return nil
}

func cmdTxnRemove(e *evalCtx) error {
	txn := e.getTxn(mandatory)
	delete(e.txns, txn.Name)
	e.results.txn = nil
	return nil
}

func cmdTxnRestart(e *evalCtx) error {
	txn := e.getTxn(mandatory)
	ts := e.getTs(txn)
	up := roachpb.NormalUserPriority
	tp := enginepb.MinTxnPriority
	txn.Restart(up, tp, ts)
	e.results.txn = txn
	return nil
}

func cmdTxnSetStatus(e *evalCtx) error {
	txn := e.getTxn(mandatory)
	status := e.getTxnStatus()
	txn.Status = status
	e.results.txn = txn
	return nil
}

func cmdTxnStep(e *evalCtx) error {
	txn := e.getTxn(mandatory)
	n := 1
	if e.hasArg("seq") {
		e.scanArg("seq", &n)
		txn.Sequence = enginepb.TxnSeq(n)
	} else {
		if e.hasArg("n") {
			e.scanArg("n", &n)
		}
		txn.Sequence += enginepb.TxnSeq(n)
	}
	e.results.txn = txn
	return nil
}

func cmdTxnUpdate(e *evalCtx) error {
	txn := e.getTxn(mandatory)
	var txnName2 string
	e.scanArg("t2", &txnName2)
	txn2, err := e.lookupTxn(txnName2)
	if err != nil {
		e.Fatalf("%v", err)
	}
	txn.Update(txn2)
	e.results.txn = txn
	return nil
}

type intentPrintingReadWriter struct {
	storage.ReadWriter
	buf *redact.StringBuilder
}

func (rw intentPrintingReadWriter) PutIntent(
	ctx context.Context, key roachpb.Key, value []byte, txnUUID uuid.UUID,
) error {
	rw.buf.Printf("called PutIntent(%v, _, %v)\n",
		key, txnUUID)
	return rw.ReadWriter.PutIntent(ctx, key, value, txnUUID)
}

func (rw intentPrintingReadWriter) ClearIntent(
	key roachpb.Key, txnDidNotUpdateMeta bool, txnUUID uuid.UUID,
) error {
	rw.buf.Printf("called ClearIntent(%v, TDNUM(%t), %v)\n",
		key, txnDidNotUpdateMeta, txnUUID)
	return rw.ReadWriter.ClearIntent(key, txnDidNotUpdateMeta, txnUUID)
}

func (e *evalCtx) tryWrapForIntentPrinting(rw storage.ReadWriter) storage.ReadWriter {
	if e.results.traceIntentWrites {
		return intentPrintingReadWriter{ReadWriter: rw, buf: e.results.buf}
	}
	return rw
}

func cmdResolveIntent(e *evalCtx) error {
	txn := e.getTxn(mandatory)
	key := e.getKey()
	status := e.getTxnStatus()
	clockWhilePending := hlc.ClockTimestamp(e.getTsWithName("clockWhilePending"))
	var targetBytes int64
	if e.hasArg("targetBytes") {
		e.scanArg("targetBytes", &targetBytes)
	}
	return e.withWriter("resolve_intent", func(rw storage.ReadWriter) error {
		return e.resolveIntent(rw, key, txn, status, clockWhilePending, targetBytes)
	})
}

func cmdResolveIntentRange(e *evalCtx) error {
	txn := e.getTxn(mandatory)
	start, end := e.getKeyRange()
	status := e.getTxnStatus()

	intent := roachpb.MakeLockUpdate(txn, roachpb.Span{Key: start, EndKey: end})
	intent.Status = status

	var maxKeys int64
	if e.hasArg("maxKeys") {
		e.scanArg("maxKeys", &maxKeys)
	}
	var targetBytes int64
	if e.hasArg("targetBytes") {
		e.scanArg("targetBytes", &targetBytes)
	}

	return e.withWriter("resolve_intent_range", func(rw storage.ReadWriter) error {
		_, _, _, _, err := storage.MVCCResolveWriteIntentRange(e.ctx, rw, e.ms, intent,
			storage.MVCCResolveWriteIntentRangeOptions{MaxKeys: maxKeys, TargetBytes: targetBytes})
		return err
	})
}

func (e *evalCtx) resolveIntent(
	rw storage.ReadWriter,
	key roachpb.Key,
	txn *roachpb.Transaction,
	resolveStatus roachpb.TransactionStatus,
	clockWhilePending hlc.ClockTimestamp,
	targetBytes int64,
) error {
	intent := roachpb.MakeLockUpdate(txn, roachpb.Span{Key: key})
	intent.Status = resolveStatus
	intent.ClockWhilePending = roachpb.ObservedTimestamp{Timestamp: clockWhilePending}
	_, _, _, err := storage.MVCCResolveWriteIntent(e.ctx, rw, e.ms, intent,
		storage.MVCCResolveWriteIntentOptions{TargetBytes: targetBytes})
	return err
}

func cmdCheckIntent(e *evalCtx) error {
	key := e.getKey()
	wantIntent := true
	if e.hasArg("none") {
		wantIntent = false
	}

	return e.withReader(func(r storage.Reader) error {
		var meta enginepb.MVCCMetadata
		iter := r.NewMVCCIterator(storage.MVCCKeyAndIntentsIterKind, storage.IterOptions{Prefix: true})
		defer iter.Close()
		iter.SeekGE(storage.MVCCKey{Key: key})
		ok, err := iter.Valid()
		if err != nil {
			return err
		}
		ok = ok && iter.UnsafeKey().Timestamp.IsEmpty()
		if ok {
			if err = iter.ValueProto(&meta); err != nil {
				return err
			}
		}
		if !ok && wantIntent {
			return errors.Newf("meta: %v -> expected intent, found none", key)
		}
		if ok {
			e.results.buf.Printf("meta: %v -> %+v\n", key, &meta)
			if !wantIntent {
				return errors.Newf("meta: %v -> expected no intent, found one", key)
			}
		}
		return nil
	})
}

func cmdAddLock(e *evalCtx) error {
	txn := e.getTxn(mandatory)
	key := e.getKey()
	e.locks[string(key)] = txn
	return nil
}

func cmdClear(e *evalCtx) error {
	key := e.getKey()
	ts := e.getTs(nil)
	return e.withWriter("clear", func(rw storage.ReadWriter) error {
		return rw.ClearMVCC(storage.MVCCKey{Key: key, Timestamp: ts})
	})
}

func cmdClearRange(e *evalCtx) error {
	key, endKey := e.getKeyRange()
	return e.withWriter("clear_range", func(rw storage.ReadWriter) error {
		// NB: We can't test ClearRawRange or ClearRangeUsingHeuristic here, because
		// it does not handle separated intents.
		if clearRangeUsingIter {
			return rw.ClearMVCCIteratorRange(key, endKey, true, true)
		}
		return rw.ClearMVCCRange(key, endKey, true, true)
	})
}

func cmdClearRangeKey(e *evalCtx) error {
	key, endKey := e.getKeyRange()
	ts := e.getTs(nil)
	return e.withWriter("clear_rangekey", func(rw storage.ReadWriter) error {
		return rw.ClearMVCCRangeKey(storage.MVCCRangeKey{StartKey: key, EndKey: endKey, Timestamp: ts})
	})
}

func cmdClearTimeRange(e *evalCtx) error {
	var clearRangeThreshold, maxBatchSize, maxBatchByteSize int
	key, endKey := e.getKeyRange()
	ts := e.getTs(nil)
	targetTs := e.getTsWithName("targetTs")
	if e.hasArg("clearRangeThreshold") {
		e.scanArg("clearRangeThreshold", &clearRangeThreshold)
	}
	if e.hasArg("maxBatchSize") {
		e.scanArg("maxBatchSize", &maxBatchSize)
	}
	if e.hasArg("maxBatchByteSize") {
		e.scanArg("maxBatchByteSize", &maxBatchByteSize)
	}

	// NB: Must use a batch, since it requires consistent iterators.
	batch := e.engine.NewBatch()
	defer batch.Close()

	rw, leftPeekBound, rightPeekBound := e.metamorphicPeekBounds(batch, key, endKey)
	resume, err := storage.MVCCClearTimeRange(e.ctx, rw, e.ms, key, endKey, targetTs, ts,
		leftPeekBound, rightPeekBound, clearRangeThreshold, int64(maxBatchSize), int64(maxBatchByteSize))
	if err != nil {
		return err
	}
	if err := batch.Commit(false); err != nil {
		return err
	}
	if resume != nil {
		e.results.buf.Printf("clear_time_range: resume=%s\n", resume)
	}
	return nil
}

func cmdGCClearRange(e *evalCtx) error {
	key, endKey := e.getKeyRange()
	gcTs := e.getTs(nil)
	return e.withWriter("gc_clear_range", func(rw storage.ReadWriter) error {
		cms, err := storage.ComputeStats(rw, key, endKey, 100e9)
		require.NoError(e.t, err, "failed to compute range stats")
		return storage.MVCCGarbageCollectWholeRange(e.ctx, rw, e.ms, key, endKey, gcTs, cms)
	})
}

func cmdGCPointsClearRange(e *evalCtx) error {
	key, endKey := e.getKeyRange()
	gcTs := e.getTs(nil)
	startTs := e.getTsWithName("startTs")
	return e.withWriter("gc_clear_range", func(rw storage.ReadWriter) error {
		return storage.MVCCGarbageCollectPointsWithClearRange(e.ctx, rw, e.ms, key, endKey, startTs, gcTs)
	})
}

func cmdCPut(e *evalCtx) error {
	txn := e.getTxn(optional)
	ts := e.getTs(txn)
	localTs := hlc.ClockTimestamp(e.getTsWithName("localTs"))

	key := e.getKey()
	val := e.getVal()
	// Condition val is optional.
	var expVal []byte
	if e.hasArg("cond") {
		rexpVal := e.getValInternal("cond")
		expVal = rexpVal.TagAndDataBytes()
	}
	behavior := storage.CPutFailIfMissing
	if e.hasArg("allow_missing") {
		behavior = storage.CPutAllowIfMissing
	}
	resolve, resolveStatus := e.getResolve()

	return e.withWriter("cput", func(rw storage.ReadWriter) error {
		if err := storage.MVCCConditionalPut(e.ctx, rw, e.ms, key, ts, localTs, val, expVal, behavior, txn); err != nil {
			return err
		}
		if resolve {
			return e.resolveIntent(rw, key, txn, resolveStatus, hlc.ClockTimestamp{}, 0)
		}
		return nil
	})
}

func cmdInitPut(e *evalCtx) error {
	txn := e.getTxn(optional)
	ts := e.getTs(txn)
	localTs := hlc.ClockTimestamp(e.getTsWithName("localTs"))

	key := e.getKey()
	val := e.getVal()
	failOnTombstones := e.hasArg("failOnTombstones")
	resolve, resolveStatus := e.getResolve()

	return e.withWriter("initput", func(rw storage.ReadWriter) error {
		if err := storage.MVCCInitPut(e.ctx, rw, e.ms, key, ts, localTs, val, failOnTombstones, txn); err != nil {
			return err
		}
		if resolve {
			return e.resolveIntent(rw, key, txn, resolveStatus, hlc.ClockTimestamp{}, 0)
		}
		return nil
	})
}

func cmdDelete(e *evalCtx) error {
	txn := e.getTxn(optional)
	key := e.getKey()
	ts := e.getTs(txn)
	localTs := hlc.ClockTimestamp(e.getTsWithName("localTs"))
	resolve, resolveStatus := e.getResolve()
	return e.withWriter("del", func(rw storage.ReadWriter) error {
		foundKey, err := storage.MVCCDelete(e.ctx, rw, e.ms, key, ts, localTs, txn)
		if err == nil || errors.HasType(err, &kvpb.WriteTooOldError{}) {
			// We want to output foundKey even if a WriteTooOldError is returned,
			// since the error may be swallowed/deferred during evaluation.
			e.results.buf.Printf("del: %v: found key %v\n", key, foundKey)
		}
		if err != nil {
			return err
		}
		if resolve {
			return e.resolveIntent(rw, key, txn, resolveStatus, hlc.ClockTimestamp{}, 0)
		}
		return nil
	})
}

func cmdDeleteRange(e *evalCtx) error {
	txn := e.getTxn(optional)
	key, endKey := e.getKeyRange()
	ts := e.getTs(txn)
	localTs := hlc.ClockTimestamp(e.getTsWithName("localTs"))
	returnKeys := e.hasArg("returnKeys")
	max := 0
	if e.hasArg("max") {
		e.scanArg("max", &max)
	}

	resolve, resolveStatus := e.getResolve()
	return e.withWriter("del_range", func(rw storage.ReadWriter) error {
		deleted, resumeSpan, num, err := storage.MVCCDeleteRange(
			e.ctx, rw, e.ms, key, endKey, int64(max), ts, localTs, txn, returnKeys)
		if err != nil {
			return err
		}
		e.results.buf.Printf("del_range: %v-%v -> deleted %d key(s)\n", key, endKey, num)
		for _, key := range deleted {
			e.results.buf.Printf("del_range: returned %v\n", key)
		}
		if resumeSpan != nil {
			e.results.buf.Printf("del_range: resume span [%s,%s)\n", resumeSpan.Key, resumeSpan.EndKey)
		}

		if resolve {
			return e.resolveIntent(rw, key, txn, resolveStatus, hlc.ClockTimestamp{}, 0)
		}
		return nil
	})
}

func cmdDeleteRangeTombstone(e *evalCtx) error {
	key, endKey := e.getKeyRange()
	ts := e.getTs(nil)
	localTs := hlc.ClockTimestamp(e.getTsWithName("localTs"))
	idempotent := e.hasArg("idempotent")

	var msCovered *enginepb.MVCCStats
	if cmdDeleteRangeTombstoneKnownStats && !e.hasArg("noCoveredStats") {
		// Some tests will submit invalid MVCC range keys, where e.g. the end key is
		// before the start key -- don't attempt to compute covered stats for these
		// to avoid iterator panics.
		if key.Compare(endKey) < 0 && key.Compare(keys.LocalMax) >= 0 {
			ms, err := storage.ComputeStats(e.engine, key, endKey, ts.WallTime)
			if err != nil {
				return err
			}
			msCovered = &ms
		}
	}

	return e.withWriter("del_range_ts", func(rw storage.ReadWriter) error {
		rw, leftPeekBound, rightPeekBound := e.metamorphicPeekBounds(rw, key, endKey)
		return storage.MVCCDeleteRangeUsingTombstone(e.ctx, rw, e.ms, key, endKey, ts, localTs,
			leftPeekBound, rightPeekBound, idempotent, 0, msCovered)
	})
}

func cmdDeleteRangePredicate(e *evalCtx) error {
	key, endKey := e.getKeyRange()
	ts := e.getTs(nil)
	localTs := hlc.ClockTimestamp(e.getTsWithName("localTs"))

	max := math.MaxInt64
	if e.hasArg("max") {
		e.scanArg("max", &max)
	}

	maxBytes := math.MaxInt64
	if e.hasArg("maxBytes") {
		e.scanArg("maxBytes", &maxBytes)
	}
	predicates := kvpb.DeleteRangePredicates{
		StartTime: e.getTsWithName("startTime"),
	}
	rangeThreshold := 64
	if e.hasArg("rangeThreshold") {
		e.scanArg("rangeThreshold", &rangeThreshold)
	}
	return e.withWriter("del_range_pred", func(rw storage.ReadWriter) error {
		rw, leftPeekBound, rightPeekBound := e.metamorphicPeekBounds(rw, key, endKey)
		resumeSpan, err := storage.MVCCPredicateDeleteRange(e.ctx, rw, e.ms, key, endKey, ts, localTs,
			leftPeekBound, rightPeekBound, predicates, int64(max), int64(maxBytes), int64(rangeThreshold), 0)

		if resumeSpan != nil {
			e.results.buf.Printf("del_range_pred: resume span [%s,%s)\n", resumeSpan.Key,
				resumeSpan.EndKey)
		}
		return err
	},
	)
}

func cmdGet(e *evalCtx) error {
	txn := e.getTxn(optional)
	key := e.getKey()
	ts := e.getTs(txn)
	opts := storage.MVCCGetOptions{Txn: txn}
	if e.hasArg("inconsistent") {
		opts.Inconsistent = true
		opts.Txn = nil
	}
	if e.hasArg("skipLocked") {
		opts.SkipLocked = true
		opts.LockTable = e.newLockTableView(txn, ts)
	}
	if e.hasArg("tombstones") {
		opts.Tombstones = true
	}
	if e.hasArg("failOnMoreRecent") {
		opts.FailOnMoreRecent = true
	}
	opts.Uncertainty = uncertainty.Interval{
		GlobalLimit: e.getTsWithName("globalUncertaintyLimit"),
		LocalLimit:  hlc.ClockTimestamp(e.getTsWithName("localUncertaintyLimit")),
	}
	if opts.Txn != nil {
		if !opts.Uncertainty.GlobalLimit.IsEmpty() {
			e.Fatalf("globalUncertaintyLimit arg incompatible with txn")
		}
		opts.Uncertainty.GlobalLimit = txn.GlobalUncertaintyLimit
	}
	if e.hasArg("maxKeys") {
		e.scanArg("maxKeys", &opts.MaxKeys)
	}
	if e.hasArg("targetBytes") {
		e.scanArg("targetBytes", &opts.TargetBytes)
	}
	if e.hasArg("allowEmpty") {
		opts.AllowEmpty = true
	}

	return e.withReader(func(r storage.Reader) error {
		res, err := storage.MVCCGet(e.ctx, r, key, ts, opts)
		// NB: the error is returned below. This ensures the test can
		// ascertain no result is populated in the intent when an error
		// occurs.
		if res.Intent != nil {
			e.results.buf.Printf("get: %v -> intent {%s}\n", key, res.Intent.Txn)
		}
		if res.Value != nil {
			e.results.buf.Printf("get: %v -> %v @%v\n", key, res.Value.PrettyPrint(), res.Value.Timestamp)
		} else {
			e.results.buf.Printf("get: %v -> <no data>\n", key)
		}
		return err
	})
}

func cmdIncrement(e *evalCtx) error {
	txn := e.getTxn(optional)
	ts := e.getTs(txn)
	localTs := hlc.ClockTimestamp(e.getTsWithName("localTs"))

	key := e.getKey()
	inc := int64(1)
	if e.hasArg("inc") {
		var incI int
		e.scanArg("inc", &incI)
		inc = int64(incI)
	}

	resolve, resolveStatus := e.getResolve()

	return e.withWriter("increment", func(rw storage.ReadWriter) error {
		curVal, err := storage.MVCCIncrement(e.ctx, rw, e.ms, key, ts, localTs, txn, inc)
		if err != nil {
			return err
		}
		e.results.buf.Printf("inc: current value = %d\n", curVal)
		if resolve {
			return e.resolveIntent(rw, key, txn, resolveStatus, hlc.ClockTimestamp{}, 0)
		}
		return nil
	})
}

func cmdMerge(e *evalCtx) error {
	key := e.getKey()
	val := e.getVal()
	ts := e.getTs(nil)
	return e.withWriter("merge", func(rw storage.ReadWriter) error {
		return storage.MVCCMerge(e.ctx, rw, e.ms, key, ts, val)
	})
}

func cmdPut(e *evalCtx) error {
	txn := e.getTxn(optional)
	ts := e.getTs(txn)
	localTs := hlc.ClockTimestamp(e.getTsWithName("localTs"))

	key := e.getKey()
	val := e.getVal()

	if e.hasArg("init-checksum") {
		val.InitChecksum(key)
	}

	resolve, resolveStatus := e.getResolve()

	return e.withWriter("put", func(rw storage.ReadWriter) error {
		if err := storage.MVCCPut(e.ctx, rw, e.ms, key, ts, localTs, val, txn); err != nil {
			return err
		}
		if resolve {
			return e.resolveIntent(rw, key, txn, resolveStatus, hlc.ClockTimestamp{}, 0)
		}
		return nil
	})
}

func cmdPutBlindInline(e *evalCtx) error {
	key := e.getKey()

	var val, prev roachpb.Value
	if e.hasArg("v") {
		val = e.getValInternal("v")
		val.InitChecksum(key)
	}
	if e.hasArg("prev") {
		prev = e.getValInternal("prev")
		prev.InitChecksum(key)
	}

	return e.withWriter("put_blind_inline", func(rw storage.ReadWriter) error {
		return storage.MVCCBlindPutInlineWithPrev(e.ctx, rw, e.ms, key, val, prev)
	})
}

func cmdIsSpanEmpty(e *evalCtx) error {
	return e.withReader(func(r storage.Reader) error {
		key, endKey := e.getKeyRange()
		isEmpty, err := storage.MVCCIsSpanEmpty(e.ctx, r, storage.MVCCIsSpanEmptyOptions{
			StartKey: key,
			EndKey:   endKey,
			StartTS:  e.getTsWithName("startTs"),
			EndTS:    e.getTs(nil),
		})
		if err != nil {
			return err
		}
		e.results.buf.Printf("%t\n", isEmpty)
		return nil
	})
}

func cmdExport(e *evalCtx) error {
	key, endKey := e.getKeyRange()
	opts := storage.MVCCExportOptions{
		StartKey:           storage.MVCCKey{Key: key, Timestamp: e.getTsWithName("kTs")},
		EndKey:             endKey,
		StartTS:            e.getTsWithName("startTs"),
		EndTS:              e.getTs(nil),
		ExportAllRevisions: e.hasArg("allRevisions"),
		StopMidKey:         e.hasArg("stopMidKey"),
		FingerprintOptions: storage.MVCCExportFingerprintOptions{
			StripTenantPrefix:            e.hasArg("stripTenantPrefix"),
			StripValueChecksum:           e.hasArg("stripValueChecksum"),
			StripIndexPrefixAndTimestamp: e.hasArg("stripped"),
		},
	}
	if e.hasArg("maxIntents") {
		e.scanArg("maxIntents", &opts.MaxIntents)
	}
	if e.hasArg("targetSize") {
		e.scanArg("targetSize", &opts.TargetSize)
	}
	if e.hasArg("maxSize") {
		e.scanArg("maxSize", &opts.MaxSize)
	}
	var shouldFingerprint bool
	if e.hasArg("fingerprint") {
		shouldFingerprint = true
	}

	r := e.newReader()
	defer r.Close()

	var sstFile bytes.Buffer

	var summary kvpb.BulkOpSummary
	var resumeInfo storage.ExportRequestResumeInfo
	var fingerprint uint64
	var hasRangeKeys bool
	var err error
	if shouldFingerprint {
		summary, resumeInfo, fingerprint, hasRangeKeys, err = storage.MVCCExportFingerprint(e.ctx, e.st, r,
			opts, &sstFile)
		if err != nil {
			return err
		}
		if !hasRangeKeys {
			sstFile.Reset()
		}
		e.results.buf.Printf("export: %s", &summary)
		e.results.buf.Print(" fingerprint=true")
	} else {
		summary, resumeInfo, err = storage.MVCCExportToSST(e.ctx, e.st, r, opts, &sstFile)
		if err != nil {
			return err
		}
		e.results.buf.Printf("export: %s", &summary)
	}

	if resumeInfo.ResumeKey.Key != nil {
		e.results.buf.Printf(" resume=%s", resumeInfo.ResumeKey)
	}
	e.results.buf.Printf("\n")

	if shouldFingerprint {
		var ssts [][]byte
		if sstFile.Len() != 0 {
			ssts = append(ssts, sstFile.Bytes())
		}
		// Fingerprint the rangekeys returned as a pebble SST.
		rangekeyFingerprint, err := storage.FingerprintRangekeys(e.ctx, e.st, opts.FingerprintOptions, ssts)
		if err != nil {
			return err
		}
		fingerprint = fingerprint ^ rangekeyFingerprint
		e.results.buf.Printf("fingerprint: %d\n", fingerprint)

		// Return early, we don't need to print the point and rangekeys if we are
		// fingerprinting.
		return nil
	}

	iter, err := storage.NewMemSSTIterator(sstFile.Bytes(), false /* verify */, storage.IterOptions{
		KeyTypes:   storage.IterKeyTypePointsAndRanges,
		UpperBound: keys.MaxKey,
	})
	if err != nil {
		return err
	}
	defer iter.Close()

	var rangeStart roachpb.Key
	for iter.SeekGE(storage.NilKey); ; iter.Next() {
		if ok, err := iter.Valid(); err != nil {
			return err
		} else if !ok {
			break
		}
		hasPoint, hasRange := iter.HasPointAndRange()
		if hasRange {
			if rangeBounds := iter.RangeBounds(); !rangeBounds.Key.Equal(rangeStart) {
				rangeStart = append(rangeStart[:0], rangeBounds.Key...)
				e.results.buf.Printf("export: %s/[", rangeBounds)
				for i, version := range iter.RangeKeys().Versions {
					val, err := storage.DecodeMVCCValue(version.Value)
					if err != nil {
						return err
					}
					if i > 0 {
						e.results.buf.Printf(" ")
					}
					e.results.buf.Printf("%s=%s", version.Timestamp, val)
				}
				e.results.buf.Printf("]\n")
			}
		}
		if hasPoint {
			key := iter.UnsafeKey()
			value, err := iter.UnsafeValue()
			if err != nil {
				return err
			}
			mvccValue, err := storage.DecodeMVCCValue(value)
			if err != nil {
				return err
			}
			e.results.buf.Printf("export: %v -> %s\n", key, mvccValue)
		}
	}

	return nil
}

func cmdScan(e *evalCtx) error {
	txn := e.getTxn(optional)
	key, endKey := e.getKeyRange()
	ts := e.getTs(txn)
	opts := storage.MVCCScanOptions{Txn: txn}
	if e.hasArg("inconsistent") {
		opts.Inconsistent = true
		opts.Txn = nil
	}
	if e.hasArg("skipLocked") {
		opts.SkipLocked = true
		opts.LockTable = e.newLockTableView(txn, ts)
	}
	if e.hasArg("tombstones") {
		opts.Tombstones = true
	}
	if e.hasArg("reverse") {
		opts.Reverse = true
	}
	if e.hasArg("failOnMoreRecent") {
		opts.FailOnMoreRecent = true
	}
	opts.Uncertainty = uncertainty.Interval{
		GlobalLimit: e.getTsWithName("globalUncertaintyLimit"),
		LocalLimit:  hlc.ClockTimestamp(e.getTsWithName("localUncertaintyLimit")),
	}
	if opts.Txn != nil {
		if !opts.Uncertainty.GlobalLimit.IsEmpty() {
			e.Fatalf("globalUncertaintyLimit arg incompatible with txn")
		}
		opts.Uncertainty.GlobalLimit = txn.GlobalUncertaintyLimit
	}
	if e.hasArg("max") {
		var n int
		e.scanArg("max", &n)
		opts.MaxKeys = int64(n)
	}
	if key := "targetbytes"; e.hasArg(key) {
		var tb int
		e.scanArg(key, &tb)
		opts.TargetBytes = int64(tb)
	}
	if e.hasArg("allowEmpty") {
		opts.AllowEmpty = true
	}
	if e.hasArg("wholeRows") {
		for _, c := range e.td.CmdArgs {
			if c.Key == "wholeRows" {
				// If we have a custom value for wholeRows key, then use it,
				// otherwise, pick an arbitrary value greater than the largest
				// column family in tests.
				if len(c.Vals) > 0 {
					wholeRowsOfSize, err := strconv.ParseInt(c.Vals[0], 10, 64)
					if err != nil {
						return err
					}
					if wholeRowsOfSize < 2 {
						return errors.Newf("wholeRowOfSize value must be at least 2, got %d", wholeRowsOfSize)
					}
					opts.WholeRowsOfSize = int32(wholeRowsOfSize)
				} else {
					opts.WholeRowsOfSize = 10
				}
				break
			}
		}
	}
	return e.withReader(func(r storage.Reader) error {
		res, err := storage.MVCCScan(e.ctx, r, key, endKey, ts, opts)
		// NB: the error is returned below. This ensures the test can
		// ascertain no result is populated in the intents when an error
		// occurs.
		for _, intent := range res.Intents {
			e.results.buf.Printf("scan: intent %v {%s}\n", intent.Intent_SingleKeySpan.Key, intent.Txn)
		}
		for _, val := range res.KVs {
			e.results.buf.Printf("scan: %v -> %v @%v\n", val.Key, val.Value.PrettyPrint(), val.Value.Timestamp)
		}
		if res.ResumeSpan != nil {
			e.results.buf.Printf("scan: resume span [%s,%s) %s nextBytes=%d\n", res.ResumeSpan.Key, res.ResumeSpan.EndKey, res.ResumeReason, res.ResumeNextBytes)
		}
		if opts.TargetBytes > 0 {
			e.results.buf.Printf("scan: %d bytes (target %d)\n", res.NumBytes, opts.TargetBytes)
		}
		if len(res.KVs) == 0 {
			e.results.buf.Printf("scan: %v-%v -> <no data>\n", key, endKey)
		}
		return err
	})
}

func cmdPutRangeKey(e *evalCtx) error {
	var rangeKey storage.MVCCRangeKey
	rangeKey.StartKey, rangeKey.EndKey = e.getKeyRange()
	rangeKey.Timestamp = e.getTs(nil)
	var value storage.MVCCValue
	value.MVCCValueHeader.LocalTimestamp = hlc.ClockTimestamp(e.getTsWithName("localTs"))

	return e.withWriter("put_rangekey", func(rw storage.ReadWriter) error {
		return rw.PutMVCCRangeKey(rangeKey, value)
	})
}

func cmdIterNew(e *evalCtx) error {
	var opts storage.IterOptions
	opts.Prefix = e.hasArg("prefix")
	if e.hasArg("k") {
		opts.LowerBound, opts.UpperBound = e.getKeyRange()
	}
	if len(opts.UpperBound) == 0 {
		opts.UpperBound = keys.MaxKey
	}
	kind := storage.MVCCKeyAndIntentsIterKind
	if e.hasArg("kind") {
		var arg string
		e.scanArg("kind", &arg)
		switch arg {
		case "keys":
			kind = storage.MVCCKeyIterKind
		case "keysAndIntents":
			kind = storage.MVCCKeyAndIntentsIterKind
		default:
			return errors.Errorf("unknown iterator kind %s", arg)
		}
	}
	if e.hasArg("types") {
		var arg string
		e.scanArg("types", &arg)
		switch arg {
		case "pointsOnly":
			opts.KeyTypes = storage.IterKeyTypePointsOnly
		case "pointsAndRanges":
			opts.KeyTypes = storage.IterKeyTypePointsAndRanges
		case "rangesOnly":
			opts.KeyTypes = storage.IterKeyTypeRangesOnly
		default:
			return errors.Errorf("unknown key type %s", arg)
		}
	}
	if e.hasArg("maskBelow") {
		opts.RangeKeyMaskingBelow = e.getTsWithName("maskBelow")
	}

	if e.iter != nil {
		e.iter.Close()
	}

	r := e.newReader()
	iter := r.NewMVCCIterator(kind, opts)
	iter = newMetamorphicIterator(e.t, e.metamorphicIterSeed(), iter).(storage.MVCCIterator)
	if opts.Prefix != iter.IsPrefix() {
		return errors.Errorf("prefix iterator returned IsPrefix=false")
	}

	e.iter = &iterWithCloser{iter, r.Close}
	e.iterRangeKeys.Clear()
	return nil
}

func cmdIterNewIncremental(e *evalCtx) error {
	var opts storage.MVCCIncrementalIterOptions
	if e.hasArg("k") {
		opts.StartKey, opts.EndKey = e.getKeyRange()
	}
	if len(opts.EndKey) == 0 {
		opts.EndKey = keys.MaxKey
	}

	opts.StartTime = e.getTsWithName("startTs")
	opts.EndTime = e.getTsWithName("endTs")
	if opts.EndTime.IsEmpty() {
		opts.EndTime = hlc.MaxTimestamp
	}

	if e.hasArg("types") {
		var arg string
		e.scanArg("types", &arg)
		switch arg {
		case "pointsOnly":
			opts.KeyTypes = storage.IterKeyTypePointsOnly
		case "pointsAndRanges":
			opts.KeyTypes = storage.IterKeyTypePointsAndRanges
		case "rangesOnly":
			opts.KeyTypes = storage.IterKeyTypeRangesOnly
		default:
			return errors.Errorf("unknown key type %s", arg)
		}
	}
	if e.hasArg("maskBelow") {
		opts.RangeKeyMaskingBelow = e.getTsWithName("maskBelow")
	}

	if e.hasArg("intents") {
		var arg string
		e.scanArg("intents", &arg)
		switch arg {
		case "error":
			opts.IntentPolicy = storage.MVCCIncrementalIterIntentPolicyError
		case "emit":
			opts.IntentPolicy = storage.MVCCIncrementalIterIntentPolicyEmit
		case "aggregate":
			opts.IntentPolicy = storage.MVCCIncrementalIterIntentPolicyAggregate
		default:
			return errors.Errorf("unknown intent policy %s", arg)
		}
	}

	if e.iter != nil {
		e.iter.Close()
	}

	r := e.newReader()
	it := storage.SimpleMVCCIterator(storage.NewMVCCIncrementalIterator(r, opts))
	// Can't metamorphically move the iterator around since when intents get aggregated
	// or emitted we can't undo that later at the level of the metamorphic iterator.
	if opts.IntentPolicy == storage.MVCCIncrementalIterIntentPolicyError {
		it = newMetamorphicIterator(e.t, e.metamorphicIterSeed(), it)
	}
	e.iter = &iterWithCloser{it, r.Close}
	e.iterRangeKeys.Clear()
	return nil
}

func cmdIterNewReadAsOf(e *evalCtx) error {
	if e.iter != nil {
		e.iter.Close()
	}
	var asOf hlc.Timestamp
	if e.hasArg("asOfTs") {
		asOf = e.getTsWithName("asOfTs")
	}
	opts := storage.IterOptions{
		KeyTypes:             storage.IterKeyTypePointsAndRanges,
		RangeKeyMaskingBelow: asOf}
	if e.hasArg("k") {
		opts.LowerBound, opts.UpperBound = e.getKeyRange()
	}
	if len(opts.UpperBound) == 0 {
		opts.UpperBound = keys.MaxKey
	}
	r := e.newReader()
	innerIter := newMetamorphicIterator(e.t, e.metamorphicIterSeed(), r.NewMVCCIterator(storage.MVCCKeyIterKind, opts))
	iter := &iterWithCloser{innerIter, r.Close}
	e.iter = storage.NewReadAsOfIterator(iter, asOf)
	e.iterRangeKeys.Clear()
	return nil
}

func cmdIterSeekGE(e *evalCtx) error {
	key := e.getKey()
	ts := e.getTs(nil)
	e.iter.SeekGE(storage.MVCCKey{Key: key, Timestamp: ts})
	printIter(e)
	return nil
}

func cmdIterSeekIntentGE(e *evalCtx) error {
	key := e.getKey()
	var txnName string
	e.scanArg("txn", &txnName)
	txn := e.txns[txnName]
	e.mvccIter().SeekIntentGE(key, txn.ID)
	printIter(e)
	return nil
}

func cmdIterSeekLT(e *evalCtx) error {
	key := e.getKey()
	ts := e.getTs(nil)
	e.mvccIter().SeekLT(storage.MVCCKey{Key: key, Timestamp: ts})
	printIter(e)
	return nil
}

func cmdIterNext(e *evalCtx) error {
	e.iter.Next()
	printIter(e)
	return nil
}

func cmdIterNextIgnoringTime(e *evalCtx) error {
	e.mvccIncrementalIter().NextIgnoringTime()
	printIter(e)
	return nil
}

func cmdIterNextKeyIgnoringTime(e *evalCtx) error {
	e.mvccIncrementalIter().NextKeyIgnoringTime()
	printIter(e)
	return nil
}

func cmdIterNextKey(e *evalCtx) error {
	e.iter.NextKey()
	printIter(e)
	return nil
}

func cmdIterPrev(e *evalCtx) error {
	e.mvccIter().Prev()
	printIter(e)
	return nil
}

func cmdIterScan(e *evalCtx) error {
	reverse := e.hasArg("reverse")
	// printIter will automatically check RangeKeyChanged() by comparing the
	// previous e.iterRangeKeys to the current. However, iter_scan is special in
	// that it also prints the current iterator position before stepping, so we
	// adjust e.iterRangeKeys to comply with the previous positioning operation.
	// The previous position already passed this check, so it doesn't matter that
	// we're fudging e.rangeKeys.
	if e.iter.RangeKeyChanged() {
		if e.iterRangeKeys.IsEmpty() {
			e.iterRangeKeys = storage.MVCCRangeKeyStack{
				// NB: Clone MinKey/MaxKey, since we write into these byte slices later.
				Bounds:   roachpb.Span{Key: keys.MinKey.Next().Clone(), EndKey: keys.MaxKey.Clone()},
				Versions: storage.MVCCRangeKeyVersions{{Timestamp: hlc.MinTimestamp}},
			}
		} else {
			e.iterRangeKeys.Clear()
		}
	}

	for {
		printIter(e)
		if ok, err := e.iter.Valid(); err != nil {
			return err
		} else if !ok {
			return nil
		}
		if reverse {
			e.mvccIter().Prev()
		} else {
			e.iter.Next()
		}
	}
}

func cmdSSTPut(e *evalCtx) error {
	key := e.getKey()
	ts := e.getTs(nil)
	var val roachpb.Value
	if e.hasArg("v") {
		val = e.getVal()
	}
	return e.sst().PutMVCC(storage.MVCCKey{Key: key, Timestamp: ts}, storage.MVCCValue{Value: val})
}

func cmdSSTPutRangeKey(e *evalCtx) error {
	var rangeKey storage.MVCCRangeKey
	rangeKey.StartKey, rangeKey.EndKey = e.getKeyRange()
	rangeKey.Timestamp = e.getTs(nil)
	var value storage.MVCCValue
	value.MVCCValueHeader.LocalTimestamp = hlc.ClockTimestamp(e.getTsWithName("localTs"))

	return e.sst().PutMVCCRangeKey(rangeKey, value)
}

func cmdSSTClearRange(e *evalCtx) error {
	start, end := e.getKeyRange()
	return e.sst().ClearRawRange(start, end, true /* pointKeys */, true /* rangeKeys */)
}

func cmdSSTClearRangeKey(e *evalCtx) error {
	var rangeKey storage.MVCCRangeKey
	rangeKey.StartKey, rangeKey.EndKey = e.getKeyRange()
	rangeKey.Timestamp = e.getTs(nil)

	return e.sst().ClearMVCCRangeKey(rangeKey)
}

func cmdSSTFinish(e *evalCtx) error {
	return e.finishSST()
}

func cmdSSTReset(e *evalCtx) error {
	if err := e.finishSST(); err != nil {
		return err
	}
	e.ssts = nil
	return nil
}

func cmdSSTIterNew(e *evalCtx) error {
	if e.iter != nil {
		e.iter.Close()
	}
	// Reverse the order of the SSTs, since earliers SSTs take precedence over
	// later SSTs, and we want last-write-wins.
	ssts := make([][]byte, len(e.ssts))
	for i, sst := range e.ssts {
		ssts[len(ssts)-i-1] = sst
	}
	iter, err := storage.NewMultiMemSSTIterator(ssts, sstIterVerify, storage.IterOptions{
		KeyTypes:   storage.IterKeyTypePointsAndRanges,
		UpperBound: keys.MaxKey,
	})
	if err != nil {
		return err
	}
	e.iter = newMetamorphicIterator(e.t, e.metamorphicIterSeed(), iter)
	e.iterRangeKeys.Clear()
	return nil
}

func cmdReplacePointTombstonesWithRangeTombstones(e *evalCtx) error {
	start, end := e.getKeyRange()
	return storage.ReplacePointTombstonesWithRangeTombstones(e.ctx, e.engine, e.ms, start, end)
}

func printIter(e *evalCtx) {
	e.results.buf.Printf("%s:", e.td.Cmd)
	defer e.results.buf.Printf("\n")

	ignoringTime := strings.HasSuffix(e.td.Cmd, "_ignoring_time")

	ok, err := e.iter.Valid()
	if err != nil {
		e.results.buf.Printf(" err=%v", err)
		return
	}
	if !ok {
		e.results.buf.Print(" .")
		e.iterRangeKeys.Clear()
		return
	}
	hasPoint, hasRange := e.iter.HasPointAndRange()
	maybeIIT := e.tryMVCCIncrementalIter()
	if !hasPoint && !hasRange && (maybeIIT == nil || maybeIIT.RangeKeysIgnoringTime().IsEmpty()) {
		e.t.Fatalf("valid iterator at %s without point nor range keys", e.iter.UnsafeKey())
	}

	checkValErr := func(v []byte, err error) []byte {
		if err != nil {
			e.Fatalf("%v", err)
		}
		return v
	}
	if hasPoint {
		if !e.iter.UnsafeKey().IsValue() {
			meta := enginepb.MVCCMetadata{}
			if err := protoutil.Unmarshal(checkValErr(e.iter.UnsafeValue()), &meta); err != nil {
				e.Fatalf("%v", err)
			}
			e.results.buf.Printf(" %s=%+v", e.iter.UnsafeKey(), &meta)
		} else {
			value, err := storage.DecodeMVCCValue(checkValErr(e.iter.UnsafeValue()))
			if err != nil {
				e.Fatalf("%v", err)
			}
			e.results.buf.Printf(" %s=%s", e.iter.UnsafeKey(), value)
		}
	}

	if hasRange {
		rangeKeys := e.iter.RangeKeys()
		e.results.buf.Printf(" %s/[", rangeKeys.Bounds)
		for i, version := range rangeKeys.Versions {
			value, err := storage.DecodeMVCCValue(version.Value)
			if err != nil {
				e.Fatalf("%v", err)
			}
			if i > 0 {
				e.results.buf.Printf(" ")
			}
			e.results.buf.Printf("%s=%s", version.Timestamp, value)
		}
		e.results.buf.Printf("]")
	}

	var rangeKeysIgnoringTime storage.MVCCRangeKeyStack
	if maybeIIT != nil {
		rangeKeysIgnoringTime = maybeIIT.RangeKeysIgnoringTime()
	}
	if ignoringTime && !rangeKeysIgnoringTime.IsEmpty() && !rangeKeysIgnoringTime.Equal(e.iter.RangeKeys()) {
		e.results.buf.Printf(" (+%s/[", rangeKeysIgnoringTime.Bounds)
		for i, version := range rangeKeysIgnoringTime.Versions {
			value, err := storage.DecodeMVCCValue(version.Value)
			if err != nil {
				e.Fatalf("%v", err)
			}
			if i > 0 {
				e.results.buf.Printf(" ")
			}
			e.results.buf.Printf("%s=%s", version.Timestamp, value)
		}
		e.results.buf.Printf("]")
		if e.mvccIncrementalIter().RangeKeyChangedIgnoringTime() {
			e.results.buf.Printf(" !")
		}
		e.results.buf.Printf(")")
	}

	if checkAndUpdateRangeKeyChanged(e) {
		e.results.buf.Printf(" !")
	}
}

func rangeKeysIfExist(it storage.SimpleMVCCIterator) storage.MVCCRangeKeyStack {
	if valid, err := it.Valid(); !valid || err != nil {
		return storage.MVCCRangeKeyStack{}
	} else if _, hasRange := it.HasPointAndRange(); !hasRange {
		return storage.MVCCRangeKeyStack{}
	}
	return it.RangeKeys()
}

func checkAndUpdateRangeKeyChanged(e *evalCtx) bool {
	rangeKeyChanged := e.iter.RangeKeyChanged()
	rangeKeys := rangeKeysIfExist(e.iter)

	if incrIter := e.tryMVCCIncrementalIter(); incrIter != nil {
		// For MVCCIncrementalIterator, make sure RangeKeyChangedIgnoringTime() fires
		// whenever RangeKeyChanged() does. The inverse is not true.
		rangeKeyChangedIgnoringTime := incrIter.RangeKeyChangedIgnoringTime()
		if rangeKeyChanged && !rangeKeyChangedIgnoringTime {
			e.t.Fatalf("RangeKeyChanged=%t but RangeKeyChangedIgnoringTime=%t",
				rangeKeyChanged, rangeKeyChangedIgnoringTime)
		}
		// If RangeKeyChangedIgnoringTime() fires, and RangeKeyChanged() doesn't,
		// then RangeKeys() must be empty.
		if rangeKeyChangedIgnoringTime && !rangeKeyChanged && !rangeKeys.IsEmpty() {
			e.t.Fatalf("RangeKeyChangedIgnoringTime without RangeKeyChanged, but RangeKeys not empty")
		}
	}

	if rangeKeyChanged != !rangeKeys.Equal(e.iterRangeKeys) {
		e.t.Fatalf("incorrect RangeKeyChanged=%t (was:%s is:%s) at %s",
			rangeKeyChanged, e.iterRangeKeys, rangeKeys, e.td.Pos)
	}
	rangeKeys.CloneInto(&e.iterRangeKeys)
	return rangeKeyChanged
}

// formatStats formats MVCC stats.
func formatStats(ms enginepb.MVCCStats, delta bool) string {
	// Split stats into field pairs. Subindex 1 is key, 2 is value.
	fields := regexp.MustCompile(`(\w+):(-?\d+)`).FindAllStringSubmatch(ms.String(), -1)

	// Sort some fields in preferred order, keeping the rest as-is at the end.
	//
	// TODO(erikgrinaker): Consider just reordering the MVCCStats struct fields
	// instead, which determines the order of MVCCStats.String().
	order := []string{"key_count", "key_bytes", "val_count", "val_bytes",
		"range_key_count", "range_key_bytes", "range_val_count", "range_val_bytes",
		"live_count", "live_bytes", "gc_bytes_age",
		"intent_count", "intent_bytes", "separated_intent_count", "intent_age"}
	sort.SliceStable(fields, func(i, j int) bool {
		for _, name := range order {
			if fields[i][1] == name {
				return true
			} else if fields[j][1] == name {
				return false
			}
		}
		return false
	})

	// Format and output fields.
	var s string
	for _, field := range fields {
		key, value := field[1], field[2]

		// Always skip zero-valued fields and LastUpdateNanos.
		if value == "0" || key == "last_update_nanos" {
			continue
		}

		if len(s) > 0 {
			s += " "
		}
		s += key + "="
		if delta && value[0] != '-' {
			s += "+" // prefix unsigned deltas with +
		}
		s += value
	}
	if len(s) == 0 && delta {
		return "no change"
	}
	return s
}

// evalCtx stored the current state of the environment of a running
// script.
type evalCtx struct {
	results struct {
		buf               *redact.StringBuilder
		txn               *roachpb.Transaction
		traceIntentWrites bool
	}
	ctx               context.Context
	st                *cluster.Settings
	engine            storage.Engine
	noMetamorphicIter bool // never instantiate metamorphicIterator
	iter              storage.SimpleMVCCIterator
	iterRangeKeys     storage.MVCCRangeKeyStack
	t                 *testing.T
	td                *datadriven.TestData
	txns              map[string]*roachpb.Transaction
	txnCounter        uint128.Uint128
	locks             map[string]*roachpb.Transaction
	ms                *enginepb.MVCCStats
	sstWriter         *storage.SSTWriter
	sstFile           *storage.MemObject
	ssts              [][]byte
}

func newEvalCtx(ctx context.Context, engine storage.Engine) *evalCtx {
	return &evalCtx{
		ctx:        ctx,
		st:         cluster.MakeTestingClusterSettings(),
		engine:     engine,
		txns:       make(map[string]*roachpb.Transaction),
		txnCounter: uint128.FromInts(0, 1),
		locks:      make(map[string]*roachpb.Transaction),
	}
}

func (e *evalCtx) close() {
	if e.iter != nil {
		e.iter.Close()
	}
	// engine is passed in, so it's the caller's responsibility to close it.
}

func (e *evalCtx) metamorphicIterSeed() int64 {
	if e.noMetamorphicIter {
		return 0
	}
	return int64(metamorphicIteratorSeed)
}

func (e *evalCtx) getTxnStatus() roachpb.TransactionStatus {
	status := roachpb.COMMITTED
	if e.hasArg("status") {
		var sn string
		e.scanArg("status", &sn)
		s, ok := roachpb.TransactionStatus_value[sn]
		if !ok {
			e.Fatalf("invalid status: %s", sn)
		}
		status = roachpb.TransactionStatus(s)
	}
	return status
}

func (e *evalCtx) scanArg(key string, dests ...interface{}) {
	e.t.Helper()
	e.td.ScanArgs(e.t, key, dests...)
}

func (e *evalCtx) hasArg(key string) bool {
	for _, c := range e.td.CmdArgs {
		if c.Key == key {
			return true
		}
	}
	return false
}

func (e *evalCtx) Fatalf(format string, args ...interface{}) {
	e.t.Helper()
	e.td.Fatalf(e.t, format, args...)
}

func (e *evalCtx) getResolve() (bool, roachpb.TransactionStatus) {
	e.t.Helper()
	if !e.hasArg("resolve") {
		return false, roachpb.PENDING
	}
	return true, e.getTxnStatus()
}

func (e *evalCtx) getTs(txn *roachpb.Transaction) hlc.Timestamp {
	return e.getTsWithTxnAndName(txn, "ts")
}

func (e *evalCtx) getTsWithName(name string) hlc.Timestamp {
	return e.getTsWithTxnAndName(nil, name)
}

func (e *evalCtx) getTsWithTxnAndName(txn *roachpb.Transaction, name string) hlc.Timestamp {
	var ts hlc.Timestamp
	if txn != nil {
		ts = txn.ReadTimestamp
	}
	if !e.hasArg(name) {
		return ts
	}
	var tsS string
	e.scanArg(name, &tsS)
	ts, err := hlc.ParseTimestamp(tsS)
	if err != nil {
		e.Fatalf("%v", err)
	}
	return ts
}

type optArg int

const (
	optional optArg = iota
	mandatory
)

func (e *evalCtx) getList(argName string) []string {
	for _, c := range e.td.CmdArgs {
		if c.Key == argName {
			return c.Vals
		}
	}
	e.Fatalf("missing argument: %s", argName)
	return nil
}

func (e *evalCtx) getTxn(opt optArg) *roachpb.Transaction {
	e.t.Helper()
	if opt == optional && (e.hasArg("notxn") || !e.hasArg("t")) {
		return nil
	}
	var txnName string
	e.scanArg("t", &txnName)
	txn, err := e.lookupTxn(txnName)
	if err != nil {
		e.Fatalf("%v", err)
	}
	return txn
}

// newReader returns a new (metamorphic) reader for use by a single command. The
// caller must call Close on the reader when done.
func (e *evalCtx) newReader() storage.Reader {
	switch mvccHistoriesReader {
	case "engine":
		return noopCloseReader{e.engine}
	case "readonly":
		return e.engine.NewReadOnly(storage.StandardDurability)
	case "batch":
		return e.engine.NewBatch()
	case "snapshot":
		return e.engine.NewSnapshot()
	default:
		e.t.Fatalf("unknown reader type %q", mvccHistoriesReader)
		return nil
	}
}

// withReader calls the given closure with a new reader, closing it when done.
func (e *evalCtx) withReader(fn func(storage.Reader) error) error {
	r := e.newReader()
	defer r.Close()
	return fn(r)
}

// withWriter calls the given closure with a writer. The writer is
// metamorphically chosen to be a batch, which will be committed and closed when
// done.
func (e *evalCtx) withWriter(cmd string, fn func(_ storage.ReadWriter) error) error {
	var rw storage.ReadWriter
	rw = e.engine
	var batch storage.Batch
	if e.hasArg("batched") || mvccHistoriesUseBatch {
		batch = e.engine.NewBatch()
		defer batch.Close()
		rw = batch
	}
	rw = e.tryWrapForIntentPrinting(rw)
	err := fn(rw)
	if e.hasArg("batched") {
		batchStatus := "non-empty"
		if batch.Empty() {
			batchStatus = "empty"
		}
		e.results.buf.Printf("%s: batch after write is %s\n", cmd, batchStatus)
	}
	if batch != nil {
		// WriteTooOldError is sometimes expected to leave behind a provisional
		// value at a higher timestamp. We commit this for parity with the engine.
		if err == nil || errors.HasType(err, &kvpb.WriteTooOldError{}) {
			if err := batch.Commit(true); err != nil {
				return err
			}
		}
	}
	return err
}

func (e *evalCtx) getVal() roachpb.Value { return e.getValInternal("v") }
func (e *evalCtx) getValInternal(argName string) roachpb.Value {
	var value string
	e.scanArg(argName, &value)
	var val roachpb.Value
	if e.hasArg("raw") {
		val.RawBytes = []byte(value)
	} else {
		val.SetString(value)
	}
	return val
}

func (e *evalCtx) getKey() roachpb.Key {
	e.t.Helper()
	var keyS string
	e.scanArg("k", &keyS)
	return toKey(keyS, e.getTenantCodec())
}

func (e *evalCtx) getKeyRange() (sk, ek roachpb.Key) {
	e.t.Helper()
	var keyS string
	e.scanArg("k", &keyS)
	codec := e.getTenantCodec()
	sk = toKey(keyS, codec)
	ek = sk.Next()
	if e.hasArg("end") {
		var endKeyS string
		e.scanArg("end", &endKeyS)
		ek = toKey(endKeyS, codec)
	}
	return sk, ek
}

func (e *evalCtx) getTenantCodec() keys.SQLCodec {
	if e.hasArg("tenant-prefix") {
		var tenantID int
		e.scanArg("tenant-prefix", &tenantID)
		return keys.MakeSQLCodec(roachpb.TenantID{InternalValue: uint64(tenantID)})
	}
	return keys.SystemSQLCodec
}

func (e *evalCtx) newTxn(
	txnName string, ts, globalUncertaintyLimit hlc.Timestamp, key roachpb.Key,
) (*roachpb.Transaction, error) {
	if _, ok := e.txns[txnName]; ok {
		e.Fatalf("txn %s already open", txnName)
	}
	txn := &roachpb.Transaction{
		TxnMeta: enginepb.TxnMeta{
			ID:             uuid.FromUint128(e.txnCounter),
			Key:            []byte(key),
			WriteTimestamp: ts,
			Sequence:       0,
		},
		Name:                   txnName,
		ReadTimestamp:          ts,
		GlobalUncertaintyLimit: globalUncertaintyLimit,
		Status:                 roachpb.PENDING,
	}
	e.txnCounter = e.txnCounter.Add(1)
	e.txns[txnName] = txn
	return txn, nil
}

func (e *evalCtx) sst() *storage.SSTWriter {
	if e.sstWriter == nil {
		e.sstFile = &storage.MemObject{}
		w := storage.MakeIngestionSSTWriter(e.ctx, e.st, e.sstFile)
		e.sstWriter = &w
	}
	return e.sstWriter
}

func (e *evalCtx) finishSST() error {
	if e.sstWriter == nil {
		return nil
	}
	err := e.sstWriter.Finish()
	if err == nil && e.sstWriter.DataSize > 0 {
		e.ssts = append(e.ssts, e.sstFile.Bytes())
	}
	e.sstFile = nil
	e.sstWriter = nil
	return err
}

func (e *evalCtx) closeSST() {
	if e.sstWriter == nil {
		return
	}
	e.sstWriter.Close()
	e.sstFile = nil
	e.sstWriter = nil
}

func (e *evalCtx) lookupTxn(txnName string) (*roachpb.Transaction, error) {
	txn, ok := e.txns[txnName]
	if !ok {
		e.Fatalf("txn %s not open", txnName)
	}
	return txn, nil
}

func (e *evalCtx) newLockTableView(
	txn *roachpb.Transaction, ts hlc.Timestamp,
) storage.LockTableView {
	return &mockLockTableView{locks: e.locks, txn: txn, ts: ts}
}

// mockLockTableView is a mock implementation of LockTableView.
type mockLockTableView struct {
	locks map[string]*roachpb.Transaction
	txn   *roachpb.Transaction
	ts    hlc.Timestamp
}

func (lt *mockLockTableView) IsKeyLockedByConflictingTxn(
	k roachpb.Key, s lock.Strength,
) (bool, *enginepb.TxnMeta) {
	holder, ok := lt.locks[string(k)]
	if !ok {
		return false, nil
	}
	if lt.txn != nil && lt.txn.ID == holder.ID {
		return false, nil
	}
	if s == lock.None && lt.ts.Less(holder.WriteTimestamp) {
		return false, nil
	}
	return true, &holder.TxnMeta
}

func (e *evalCtx) visitWrappedIters(fn func(it storage.SimpleMVCCIterator) (done bool)) {
	iter := e.iter
	if iter == nil {
		return
	}
	for {
		if fn(iter) {
			return
		}
		if i, ok := iter.(*iterWithCloser); ok {
			iter = i.SimpleMVCCIterator
			continue
		}
		if i, ok := iter.(*metamorphicIterator); ok {
			iter = i.it
			continue
		}
		if i, ok := iter.(*metamorphicMVCCIterator); ok {
			iter = i.it
			continue
		}
		if i, ok := iter.(*metamorphicMVCCIncrementalIterator); ok {
			iter = i.it
			continue
		}
		return // unwrapped all we knew to unwrap
	}
}

func (e *evalCtx) mvccIter() storage.MVCCIterator {
	var iter storage.MVCCIterator
	e.visitWrappedIters(func(it storage.SimpleMVCCIterator) (done bool) {
		iter, done = it.(storage.MVCCIterator)
		return done
	})
	require.NotNil(e.t, iter, "need an MVCC iterator")
	return iter
}

func (e *evalCtx) mvccIncrementalIter() mvccIncrementalIteratorI {
	iter := e.tryMVCCIncrementalIter()
	require.NotNil(e.t, iter, "need an MVCCIncrementalIterator")
	return iter
}

type mvccIncrementalIteratorI interface {
	storage.SimpleMVCCIterator
	RangeKeysIgnoringTime() storage.MVCCRangeKeyStack
	RangeKeyChangedIgnoringTime() bool
	NextIgnoringTime()
	NextKeyIgnoringTime()
	TryGetIntentError() error
}

var _ mvccIncrementalIteratorI = (*storage.MVCCIncrementalIterator)(nil)

// tryMVCCIncrementalIter unwraps an MVCCIncrementalIterator, if there is one.
// This does not return the verbatim *MVCCIncrementalIterator but an interface,
// since we are usually wrapping in a metamorphicIterator which injects extra
// movement and thus needs to mask RangeKeyChanged{,IgnoringTime}.
func (e *evalCtx) tryMVCCIncrementalIter() mvccIncrementalIteratorI {
	var iter mvccIncrementalIteratorI
	e.visitWrappedIters(func(it storage.SimpleMVCCIterator) (done bool) {
		iter, done = it.(mvccIncrementalIteratorI)
		return done
	})
	return iter
}

func (e *evalCtx) iterErr() error {
	if e.iter == nil {
		return nil
	}
	if _, err := e.iter.Valid(); err != nil {
		return err
	}
	if mvccIncrementalIter := e.tryMVCCIncrementalIter(); mvccIncrementalIter != nil {
		if err := mvccIncrementalIter.TryGetIntentError(); err != nil {
			return err
		}
	}
	return nil
}

// metamorphicPeekBounds generates MVCC range key peek bounds for a command
// based on its keyspan, and enables spanset assertions for the ReadWriter.
func (e *evalCtx) metamorphicPeekBounds(
	rw storage.ReadWriter, start, end roachpb.Key,
) (storage.ReadWriter, roachpb.Key, roachpb.Key) {
	leftPeekBound, rightPeekBound := start.Prevish(8), end.Next()
	if end == nil {
		rightPeekBound = nil
	}

	switch mvccHistoriesPeekBounds {
	case "none":
		leftPeekBound, rightPeekBound = nil, nil
	case "left":
		rightPeekBound = nil
	case "right":
		leftPeekBound = nil
	case "both":
	default:
		e.t.Fatalf("invalid peek bound kind %q", mvccHistoriesPeekBounds)
		return nil, nil, nil
	}

	if leftPeekBound != nil || rightPeekBound != nil {
		ss := &spanset.SpanSet{}
		ss.AddNonMVCC(spanset.SpanReadWrite, roachpb.Span{Key: start, EndKey: end})
		peekSpan := roachpb.Span{Key: leftPeekBound, EndKey: rightPeekBound}
		if peekSpan.Key == nil {
			peekSpan.Key = keys.LocalMax
		}
		if peekSpan.EndKey == nil {
			peekSpan.EndKey = keys.MaxKey
		}
		ss.AddNonMVCC(spanset.SpanReadOnly, peekSpan)
		rw = spanset.NewReadWriterAt(rw, ss, hlc.Timestamp{})
	}

	return rw, leftPeekBound, rightPeekBound
}

func toKey(s string, sqlCodec keys.SQLCodec) roachpb.Key {
	if len(s) == 0 {
		return roachpb.Key(s)
	}
	switch s[0] {
	case '+':
		return roachpb.Key(s[1:]).Next()
	case '=':
		return roachpb.Key(s[1:])
	case '-':
		return roachpb.Key(s[1:]).PrefixEnd()
	case '%':
		return append(keys.LocalRangePrefix, s[1:]...)
	case '/':
		var pk string
		var columnFamilyID uint64
		var err error
		parts := strings.Split(s[1:], "/")
		switch len(parts) {
		case 2:
			if columnFamilyID, err = strconv.ParseUint(parts[1], 10, 32); err != nil {
				panic(fmt.Sprintf("invalid column family ID %s in row key %s: %s", parts[1], s, err))
			}
			fallthrough
		case 1:
			pk = parts[0]
		default:
			panic(fmt.Sprintf("expected at most one / separator in row key %s", s))
		}

		var colMap catalog.TableColMap
		colMap.Set(0, 0)
		key := sqlCodec.IndexPrefix(1, 1)
		key, _, err = rowenc.EncodeColumns([]descpb.ColumnID{0}, nil /* directions */, colMap, []tree.Datum{tree.NewDString(pk)}, key)
		if err != nil {
			panic(err)
		}
		key = keys.MakeFamilyKey(key, uint32(columnFamilyID))
		return key
	default:
		return roachpb.Key(s)
	}
}

// iterWithCloser will call the given closer when the iterator
// is closed.
type iterWithCloser struct {
	storage.SimpleMVCCIterator
	closer func()
}

func (i *iterWithCloser) Close() {
	i.SimpleMVCCIterator.Close()
	if i.closer != nil {
		i.closer()
	}
}

// noopCloseReader overrides Reader.Close() with a noop.
type noopCloseReader struct {
	storage.Reader
}

func (noopCloseReader) Close() {}
