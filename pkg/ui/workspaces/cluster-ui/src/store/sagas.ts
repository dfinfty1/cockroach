// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

import { SagaIterator } from "redux-saga";
import { all, fork } from "redux-saga/effects";

import { localStorageSaga } from "./localStorage";
import { statementsDiagnosticsSagas } from "./statementDiagnostics";
import { nodesSaga } from "./nodes";
import { jobsSaga } from "./jobs";
import { jobSaga } from "./jobDetails";
import { livenessSaga } from "./liveness";
import { sessionsSaga } from "./sessions";
import { terminateSaga } from "./terminateQuery";
import { notifificationsSaga } from "./notifications";
import { sqlStatsSaga } from "./sqlStats";
import { sqlDetailsStatsSaga } from "./statementDetails";
import { indexStatsSaga } from "./indexStats/indexStats.sagas";
import { clusterLocksSaga } from "./clusterLocks/clusterLocks.saga";
import { insightsSaga } from "./insights/insights.sagas";

export function* sagas(cacheInvalidationPeriod?: number): SagaIterator {
  yield all([
    fork(localStorageSaga),
    fork(statementsDiagnosticsSagas, cacheInvalidationPeriod),
    fork(nodesSaga, cacheInvalidationPeriod),
    fork(livenessSaga, cacheInvalidationPeriod),
    fork(insightsSaga),
    fork(jobsSaga),
    fork(jobSaga),
    fork(sessionsSaga),
    fork(terminateSaga),
    fork(notifificationsSaga),
    fork(sqlStatsSaga),
    fork(sqlDetailsStatsSaga),
    fork(indexStatsSaga),
    fork(clusterLocksSaga),
  ]);
}
