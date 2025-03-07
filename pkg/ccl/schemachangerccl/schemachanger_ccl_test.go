// Copyright 2022 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package schemachangerccl

import (
	gosql "database/sql"
	"flag"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/build/bazel"
	"github.com/cockroachdb/cockroach/pkg/ccl/multiregionccl/multiregionccltestutils"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scexec"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/sctest"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/skip"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
)

// Used for saving corpus information in TestGenerateCorpus
var corpusPath string

func init() {
	flag.StringVar(&corpusPath, "declarative-corpus", "", "Path to the corpus file")
}

func newCluster(t *testing.T, knobs *scexec.TestingKnobs) (*gosql.DB, func()) {
	_, sqlDB, cleanup := multiregionccltestutils.TestingCreateMultiRegionCluster(
		t, 3 /* numServers */, base.TestingKnobs{
			SQLDeclarativeSchemaChanger: knobs,
			JobsTestingKnobs:            jobs.NewTestingKnobsWithShortIntervals(),
		},
	)
	return sqlDB, cleanup
}

func sharedTestdata(t *testing.T) string {
	testdataDir := "../../sql/schemachanger/testdata/end_to_end"
	if bazel.BuiltWithBazel() {
		runfile, err := bazel.Runfile("pkg/sql/schemachanger/testdata/end_to_end")
		if err != nil {
			t.Fatal(err)
		}
		testdataDir = runfile
	}
	return testdataDir
}

func endToEndPath(t *testing.T) string {
	return testutils.TestDataPath(t, "end_to_end")
}

func TestSchemaChangerSideEffects(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	sctest.EndToEndSideEffects(t, endToEndPath(t), newCluster)
}

func TestBackupRestoreCCL(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	sctest.Backup(t, endToEndPath(t), newCluster)
}

func TestBackupRestoreNonCCL(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	sctest.Backup(t, sharedTestdata(t), sctest.SingleNodeCluster)
}

func TestRollback(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	sctest.Rollback(t, endToEndPath(t), newCluster)
}

func TestPause(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	sctest.Pause(t, endToEndPath(t), newCluster)
}

// TestGenerateCorpus generates a corpus based on the end to end test files.
func TestGenerateCorpus(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	if corpusPath == "" {
		skip.IgnoreLintf(t, "requires declarative-corpus path parameter")
	}
	sctest.GenerateSchemaChangeCorpus(t, endToEndPath(t), corpusPath, newCluster)
}

func TestDecomposeToElements(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	sctest.DecomposeToElements(t, testutils.TestDataPath(t, "decomp"), newCluster)
}
