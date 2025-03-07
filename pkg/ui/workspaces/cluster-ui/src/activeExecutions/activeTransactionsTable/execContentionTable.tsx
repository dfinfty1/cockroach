// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

import React, { useMemo } from "react";
import { SortedTable, ColumnDescriptor } from "../../sortedtable";
import { ContendedExecution, ExecutionType } from "../types";
import { Link } from "react-router-dom";
import { StatusIcon } from "../statusIcon";
import { executionsTableTitles } from "../execTableCommon";
import { DATE_FORMAT, Duration } from "../../util";

const getID = (item: ContendedExecution, execType: ExecutionType) =>
  execType === "transaction"
    ? item.transactionExecutionID
    : item.statementExecutionID;

export function makeContentionColumns(
  execType: ExecutionType,
): ColumnDescriptor<ContendedExecution>[] {
  const columns: ColumnDescriptor<ContendedExecution>[] = [
    {
      name: "executionID",
      title: executionsTableTitles.executionID(execType),
      cell: item => (
        <Link
          to={`/execution/${execType.toLowerCase()}/${getID(item, execType)}`}
        >
          {getID(item, execType)}
        </Link>
      ),
      sort: item => getID(item, execType),
      alwaysShow: true,
    },
    {
      name: "mostRecentStatement",
      title: executionsTableTitles.mostRecentStatement(execType),
      cell: item => (
        <Link to={`/execution/statement/${item.statementExecutionID}`}>
          {item.query}
        </Link>
      ),
      sort: item => item.query,
    },
    {
      name: "status",
      title: executionsTableTitles.status(execType),
      cell: item => (
        <span>
          <StatusIcon status={item.status} />
          {item.status}
        </span>
      ),
      sort: item => item.status,
    },
    {
      name: "startTime",
      title: executionsTableTitles.startTime(execType),
      cell: item => item.start.format(DATE_FORMAT),
      sort: item => item.start.unix(),
    },
    {
      name: "timeSpentBlocking",
      title: executionsTableTitles.timeSpentBlocking(execType),
      cell: item => Duration(item.contentionTime.asSeconds() * 1e9),
      sort: item => item.contentionTime.asSeconds(),
    },
  ];
  return columns;
}

interface ContentionTableProps {
  data: ContendedExecution[];
  execType: ExecutionType;
}

const txnColumns = makeContentionColumns("transaction");
const stmtColumns = makeContentionColumns("statement");

export const ExecutionContentionTable: React.FC<
  ContentionTableProps
> = props => {
  const columns = props.execType === "statement" ? stmtColumns : txnColumns;
  return <SortedTable columns={columns} {...props} />;
};
