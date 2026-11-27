package store

import (
	"context"
	"database/sql"
	"encoding/json"
)

type SubmissionKey struct {
	CredentialDigest string
	RunID            string
	TurnID           string
}

type Submission struct {
	Sequence   int64
	Status     string
	Model      string
	ResponseID string
	CallIDs    []string
}

func (s *Store) Submission(ctx context.Context, key SubmissionKey) (Submission, error) {
	var submission Submission
	var callIDs string
	err := s.db.QueryRowContext(ctx, `SELECT sequence,status,model,response_id,call_ids FROM gateway_submissions
 WHERE credential_digest=? AND run_id=? AND turn_id=?`, key.CredentialDigest, key.RunID, key.TurnID).
		Scan(&submission.Sequence, &submission.Status, &submission.Model, &submission.ResponseID, &callIDs)
	if err != nil {
		return Submission{}, missing(err)
	}
	return submission, json.Unmarshal([]byte(callIDs), &submission.CallIDs)
}

// CompareSubmission establishes the durable boundary before an upstream
// write and refuses to advance a record another request has already changed.
func (s *Store) CompareSubmission(ctx context.Context, key SubmissionKey, expectedSequence int64, next Submission) (Submission, error) {
	callIDs, err := json.Marshal(next.CallIDs)
	if err != nil {
		return Submission{}, err
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := authenticateCredential(ctx, tx, key.CredentialDigest); err != nil {
			return err
		}
		if expectedSequence == 0 {
			result, err := tx.ExecContext(ctx, `INSERT INTO gateway_submissions
 (credential_digest,run_id,turn_id,sequence,status,model,response_id,call_ids)
 VALUES (?,?,?,1,?,?,?,?) ON CONFLICT DO NOTHING`, key.CredentialDigest, key.RunID, key.TurnID,
				next.Status, next.Model, next.ResponseID, string(callIDs))
			return affected(result, err, ErrConflict)
		}
		result, err := tx.ExecContext(ctx, `UPDATE gateway_submissions SET sequence=sequence+1,status=?,response_id=?,call_ids=?
 WHERE credential_digest=? AND run_id=? AND turn_id=? AND sequence=? AND model=?`, next.Status, next.ResponseID, string(callIDs),
			key.CredentialDigest, key.RunID, key.TurnID, expectedSequence, next.Model)
		return affected(result, err, ErrConflict)
	})
	if err != nil {
		return Submission{}, err
	}
	next.Sequence = expectedSequence + 1
	return next, nil
}

func (s *Store) SubmissionReferences(ctx context.Context, credentialDigest, sessionDigest string, digests []string) (bool, error) {
	var owned bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		credential, err := authenticateCredential(ctx, tx, credentialDigest)
		if err != nil {
			return err
		}
		scope := GatewayCredentialScope(credential)
		for _, digest := range digests {
			var exists bool
			e := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM gateway_submission_references
 WHERE workspace_id=? AND instance_scope=? AND session_digest=? AND reference_digest=?)`,
				credential.WorkspaceID, scope, sessionDigest, digest).Scan(&exists)
			if e != nil || !exists {
				return e
			}
		}
		owned = true
		return nil
	})
	return owned, err
}

func (s *Store) RecordSubmissionReferences(ctx context.Context, credentialDigest, sessionDigest string, digests []string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		credential, err := authenticateCredential(ctx, tx, credentialDigest)
		if err != nil {
			return err
		}
		scope := GatewayCredentialScope(credential)
		for _, digest := range digests {
			if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_submission_references (workspace_id,instance_scope,session_digest,reference_digest)
 VALUES (?,?,?,?) ON CONFLICT DO NOTHING`, credential.WorkspaceID, scope, sessionDigest, digest); err != nil {
				return err
			}
		}
		return nil
	})
}
