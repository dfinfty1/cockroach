// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package sql

import (
	"context"
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/server/telemetry"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/dbdesc"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/schemadesc"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/typedesc"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/sql/sqltelemetry"
	"github.com/cockroachdb/cockroach/pkg/util/log/eventpb"
	"github.com/cockroachdb/errors"
)

type dropSchemaNode struct {
	n *tree.DropSchema
	d *dropCascadeState
}

// Use to satisfy the linter.
var _ planNode = &dropSchemaNode{n: nil}

func (p *planner) DropSchema(ctx context.Context, n *tree.DropSchema) (planNode, error) {
	if err := checkSchemaChangeEnabled(
		ctx,
		p.ExecCfg(),
		"DROP SCHEMA",
	); err != nil {
		return nil, err
	}

	isAdmin, err := p.HasAdminRole(ctx)
	if err != nil {
		return nil, err
	}

	d := newDropCascadeState()

	// Collect all schemas to be deleted.
	for _, schema := range n.Names {
		dbName := p.CurrentDatabase()
		if schema.ExplicitCatalog {
			dbName = schema.Catalog()
		}
		scName := schema.Schema()

		db, err := p.Descriptors().GetMutableDatabaseByName(ctx, p.txn, dbName,
			tree.DatabaseLookupFlags{Required: true})
		if err != nil {
			return nil, err
		}

		sc, err := p.Descriptors().GetSchemaByName(
			ctx, p.txn, db, scName, tree.SchemaLookupFlags{
				Required:       false,
				RequireMutable: true,
			},
		)
		if err != nil {
			return nil, err
		}
		if sc == nil {
			if n.IfExists {
				continue
			}
			return nil, pgerror.Newf(pgcode.InvalidSchemaName, "unknown schema %q", scName)
		}

		if scName == tree.PublicSchema {
			return nil, pgerror.Newf(pgcode.InvalidSchemaName, "cannot drop schema %q", scName)
		}

		switch sc.SchemaKind() {
		case catalog.SchemaPublic, catalog.SchemaVirtual, catalog.SchemaTemporary:
			return nil, pgerror.Newf(pgcode.InvalidSchemaName, "cannot drop schema %q", scName)
		case catalog.SchemaUserDefined:
			hasOwnership, err := p.HasOwnership(ctx, sc)
			if err != nil {
				return nil, err
			}
			if !(isAdmin || hasOwnership) {
				return nil, pgerror.Newf(pgcode.InsufficientPrivilege,
					"must be owner of schema %s", tree.Name(sc.GetName()))
			}
			namesBefore := len(d.objectNamesToDelete)
			fnsBefore := len(d.functionsToDelete)
			if err := d.collectObjectsInSchema(ctx, p, db, sc); err != nil {
				return nil, err
			}
			// We added some new objects to delete. Ensure that we have the correct
			// drop behavior to be doing this.
			if (namesBefore != len(d.objectNamesToDelete) || fnsBefore != len(d.functionsToDelete)) &&
				n.DropBehavior != tree.DropCascade {

				return nil, pgerror.Newf(pgcode.DependentObjectsStillExist,
					"schema %q is not empty and CASCADE was not specified", scName)
			}
			sqltelemetry.IncrementUserDefinedSchemaCounter(sqltelemetry.UserDefinedSchemaDrop)
		default:
			return nil, errors.AssertionFailedf("unknown schema kind %d", sc.SchemaKind())
		}

	}

	if err := d.resolveCollectedObjects(ctx, p); err != nil {
		return nil, err
	}

	return &dropSchemaNode{n: n, d: d}, nil
}

func (n *dropSchemaNode) startExec(params runParams) error {
	telemetry.Inc(sqltelemetry.SchemaChangeDropCounter("schema"))

	ctx := params.ctx
	p := params.p

	// Drop all collected objects.
	if err := n.d.dropAllCollectedObjects(ctx, p); err != nil {
		return err
	}

	// Queue the job to actually drop the schema.
	schemaIDs := make([]descpb.ID, len(n.d.schemasToDelete))
	for i := range n.d.schemasToDelete {
		sc := n.d.schemasToDelete[i].schema
		schemaIDs[i] = sc.GetID()
		db := n.d.schemasToDelete[i].dbDesc

		mutDesc := sc.(*schemadesc.Mutable)
		if err := p.dropSchemaImpl(ctx, db, mutDesc); err != nil {
			return err
		}
	}

	// Write out the change to the database.
	for i := range n.d.schemasToDelete {
		sc := n.d.schemasToDelete[i].schema
		db := n.d.schemasToDelete[i].dbDesc
		if err := p.writeNonDropDatabaseChange(
			ctx, db,
			fmt.Sprintf("updating parent database %s for %s", db.GetName(), sc.GetName()),
		); err != nil {
			return err
		}
	}

	// Create the job to drop the schema.
	if err := p.createDropSchemaJob(
		schemaIDs,
		n.d.getDroppedTableDetails(),
		n.d.typesToDelete,
		tree.AsStringWithFQNames(n.n, params.Ann()),
	); err != nil {
		return err
	}

	// Log Drop Schema event. This is an auditable log event and is recorded
	// in the same transaction as table descriptor update.
	for _, schemaToDelete := range n.d.schemasToDelete {
		sc := schemaToDelete.schema
		qualifiedSchemaName, err := p.getQualifiedSchemaName(params.ctx, sc)
		if err != nil {
			return err
		}

		if err := params.p.logEvent(params.ctx,
			sc.GetID(),
			&eventpb.DropSchema{
				SchemaName: qualifiedSchemaName.String(),
			}); err != nil {
			return err
		}
	}
	return nil
}

// dropSchemaImpl performs the logic of dropping a user defined schema. It does
// not create a job to perform the final cleanup of the schema.
func (p *planner) dropSchemaImpl(
	ctx context.Context, parentDB *dbdesc.Mutable, sc *schemadesc.Mutable,
) error {

	// Update parent database schemas mapping.
	delete(parentDB.Schemas, sc.GetName())

	// Update the schema descriptor as dropped.
	sc.SetDropped()

	// Populate namespace update batch.
	b := p.txn.NewBatch()
	p.dropNamespaceEntry(ctx, b, sc)

	// Remove any associated comments.
	if err := p.removeSchemaComment(ctx, sc.GetID()); err != nil {
		return err
	}

	// Write the updated descriptor.
	if err := p.writeSchemaDesc(ctx, sc); err != nil {
		return err
	}

	// Run the namespace update batch.
	return p.txn.Run(ctx, b)
}

func (p *planner) createDropSchemaJob(
	schemas []descpb.ID,
	tableDropDetails []jobspb.DroppedTableDetails,
	typesToDrop []*typedesc.Mutable,
	jobDesc string,
) error {
	typeIDs := make([]descpb.ID, 0, len(typesToDrop))
	for _, t := range typesToDrop {
		typeIDs = append(typeIDs, t.ID)
	}

	_, err := p.extendedEvalCtx.QueueJob(p.EvalContext().Ctx(), jobs.Record{
		Description:   jobDesc,
		Username:      p.User(),
		DescriptorIDs: schemas,
		Details: jobspb.SchemaChangeDetails{
			DroppedSchemas:    schemas,
			DroppedTables:     tableDropDetails,
			DroppedTypes:      typeIDs,
			DroppedDatabaseID: descpb.InvalidID,
			// The version distinction for database jobs doesn't matter for jobs that
			// drop schemas.
			FormatVersion: jobspb.DatabaseJobFormatVersion,
		},
		Progress:      jobspb.SchemaChangeProgress{},
		NonCancelable: true,
	})
	return err
}

func (p *planner) removeSchemaComment(ctx context.Context, schemaID descpb.ID) error {
	_, err := p.ExtendedEvalContext().ExecCfg.InternalExecutor.ExecEx(
		ctx,
		"delete-schema-comment",
		p.txn,
		sessiondata.InternalExecutorOverride{User: username.RootUserName()},
		"DELETE FROM system.comments WHERE type=$1 AND object_id=$2 AND sub_id=0",
		keys.SchemaCommentType,
		schemaID)

	return err
}

func (n *dropSchemaNode) Next(params runParams) (bool, error) { return false, nil }
func (n *dropSchemaNode) Values() tree.Datums                 { return tree.Datums{} }
func (n *dropSchemaNode) Close(ctx context.Context)           {}
func (n *dropSchemaNode) ReadingOwnWrites()                   {}
