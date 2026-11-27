package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := authenticateCredential(ctx, tx, key.CredentialDigest); err != nil {
			return err
		}
		return compareSubmission(ctx, tx, key, expectedSequence, next)
	})
	if err != nil {
		return Submission{}, err
	}
	next.Sequence = expectedSequence + 1
	return next, nil
}

func compareSubmission(ctx context.Context, tx *sql.Tx, key SubmissionKey, expectedSequence int64, next Submission) error {
	callIDs, err := json.Marshal(next.CallIDs)
	if err != nil {
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
}

type SubmissionReference struct {
	Digest        string
	HistoryDigest string
}

func (s *Store) SubmissionReferences(ctx context.Context, credentialDigest, sessionDigest string, references []SubmissionReference) (bool, error) {
	var owned bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		credential, err := authenticateCredential(ctx, tx, credentialDigest)
		if err != nil {
			return err
		}
		scope := GatewayCredentialScope(credential)
		for _, reference := range references {
			var exists bool
			e := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM gateway_submission_references
 WHERE workspace_id=? AND instance_scope=? AND session_digest=? AND reference_digest=?) OR EXISTS
 (SELECT 1 FROM gateway_history_references
 WHERE workspace_id=? AND container_scope=? AND grant_id=? AND grant_epoch=? AND session_digest=? AND reference_digest=?)`,
				credential.WorkspaceID, scope, sessionDigest, reference.Digest,
				credential.WorkspaceID, gatewayHistoryContainerScope(credential), credential.GrantID, credential.GrantEpoch,
				sessionDigest, reference.HistoryDigest).Scan(&exists)
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
		return recordSubmissionReferences(ctx, tx, credential, sessionDigest, digests, nil)
	})
}

type SubmittedToolResult struct {
	CallID          string
	CallFingerprint string
	Fingerprint     string
}

type SubmissionCompletion struct {
	SessionDigest     string
	References        []string
	ToolCalls         map[string]string
	ToolResults       []SubmittedToolResult
	HistoryReferences []string
}

// CompleteSubmission commits the terminal boundary and the references it proves together.
func (s *Store) CompleteSubmission(ctx context.Context, key SubmissionKey, sequence int64, next Submission, completion SubmissionCompletion) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		credential, err := authenticateCredential(ctx, tx, key.CredentialDigest)
		if err != nil {
			return err
		}
		if err := compareSubmission(ctx, tx, key, sequence, next); err != nil {
			return err
		}
		if next.Status == "completed" {
			if err := recordCompletedHistory(ctx, tx, credential, completion); err != nil {
				return err
			}
		}
		return recordSubmissionReferences(ctx, tx, credential, completion.SessionDigest, completion.References, completion.ToolCalls)
	})
}

func recordSubmissionReferences(ctx context.Context, tx *sql.Tx, credential GatewayCredential, session string, digests []string, calls map[string]string) error {
	scope := GatewayCredentialScope(credential)
	for _, digest := range digests {
		var call any
		if fingerprint, ok := calls[digest]; ok {
			call = fingerprint
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_submission_references
 (workspace_id,instance_scope,session_digest,reference_digest,tool_call_fingerprint)
 VALUES (?,?,?,?,?) ON CONFLICT DO NOTHING`, credential.WorkspaceID, scope, session, digest, call); err != nil {
			return err
		}
	}
	return nil
}

func GatewayToolHistoryDigest(call, result string) string {
	return GatewayCredentialDigest("tool-history " + call + " " + result)
}

func recordCompletedHistory(ctx context.Context, tx *sql.Tx, credential GatewayCredential, completion SubmissionCompletion) error {
	history := completion.HistoryReferences
	for _, result := range completion.ToolResults {
		var call string
		err := tx.QueryRowContext(ctx, `SELECT tool_call_fingerprint FROM gateway_submission_references
 WHERE workspace_id=? AND instance_scope=? AND session_digest=? AND reference_digest=? AND tool_call_fingerprint IS NOT NULL`,
			credential.WorkspaceID, GatewayCredentialScope(credential), completion.SessionDigest, result.CallID).Scan(&call)
		if errors.Is(err, sql.ErrNoRows) {
			continue // A historical pair does not prove a new live call.
		}
		if err != nil {
			return err
		}
		if result.CallFingerprint == "" || result.CallFingerprint == call {
			history = append(history, GatewayToolHistoryDigest(call, result.Fingerprint))
		}
	}
	for _, digest := range history {
		if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_history_references
 (workspace_id,container_scope,grant_id,grant_epoch,session_digest,reference_digest)
 VALUES (?,?,?,?,?,?) ON CONFLICT DO NOTHING`, credential.WorkspaceID, gatewayHistoryContainerScope(credential),
			credential.GrantID, credential.GrantEpoch, completion.SessionDigest, digest); err != nil {
			return err
		}
	}
	return nil
}
