package main

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/enroll"
)

/*
directory answers enroll's question — who in this workspace holds these addresses —
over auth, which owns users and memberships.

The interface is enroll's, declared by the package that needs it; the answer is
auth's, because that is whose table it is. Neither imports the other, and this file
is the only place that knows both exist.
*/
type directory struct{ repo *auth.PostgresRepository }

func (d directory) MembersByEmail(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, emails []string) (map[string]uuid.UUID, error) {
	return d.repo.MembersByEmail(ctx, tx, tenantID, emails)
}

var _ enroll.Directory = directory{}
