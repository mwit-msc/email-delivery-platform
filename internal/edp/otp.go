package edp

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const otpAttempts = 5

var otpPurpose = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)

type OTPInput struct {
	To      string `json:"to"`
	Purpose string `json:"purpose"`
}

type OTPChallenge struct {
	ID        string    `json:"id"`
	MessageID string    `json:"message_id"`
	ExpiresAt time.Time `json:"expires_at"`
	Replayed  bool      `json:"replayed"`
}

func otpDigest(secret []byte, id, app, recipient, purpose, code string) []byte {
	mac := hmac.New(sha256.New, secret)
	data, _ := json.Marshal([]string{"edp:otp:v1", id, app, recipient, purpose, code})
	mac.Write(data)
	return mac.Sum(nil)
}

func (a *App) createOTP(ctx context.Context, app string, in OTPInput, key string) (OTPChallenge, error) {
	var result OTPChallenge
	in.To = certificateRecipientKey(strings.TrimSpace(in.To))
	in.Purpose = strings.ToLower(strings.TrimSpace(in.Purpose))
	if !validAddress(in.To) || !otpPurpose.MatchString(in.Purpose) {
		return result, &HTTPError{400, "valid to address and purpose (1–64 lowercase letters, digits, dots, dashes or underscores, starting with a letter) required"}
	}
	if len(key) < 1 || len(key) > 200 {
		return result, &HTTPError{400, "Idempotency-Key is required (maximum 200 characters)"}
	}
	request, _ := json.Marshal(in)
	fingerprint := tokenHash(string(request))
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	// Match admission's application-first order. This also serializes resends
	// across purposes and across web processes without an in-memory throttle.
	var appID string
	if err = tx.QueryRow(ctx, "SELECT id FROM applications WHERE id=$1 FOR UPDATE", app).Scan(&appID); err != nil {
		return result, err
	}
	var savedHash string
	err = tx.QueryRow(ctx, "SELECT id,message_id,expires_at,payload_hash FROM otp_challenges WHERE application_id=$1 AND idempotency_key=$2", app, key).Scan(&result.ID, &result.MessageID, &result.ExpiresAt, &savedHash)
	if err == nil {
		if savedHash != fingerprint {
			return OTPChallenge{}, &HTTPError{409, "idempotency key was already used with a different payload"}
		}
		result.Replayed = true
		return result, nil
	}
	if err != pgx.ErrNoRows {
		return result, err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return result, err
	}
	var last *time.Time
	if err = tx.QueryRow(ctx, "SELECT max(created_at) FROM otp_challenges WHERE application_id=$1 AND lower(recipient)=lower($2)", app, in.To).Scan(&last); err != nil {
		return result, err
	}
	if last != nil && now.Sub(*last) < time.Minute {
		return result, &HTTPError{429, "wait 60 seconds between OTP requests to this recipient"}
	}
	number, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return result, err
	}
	code := fmt.Sprintf("%06d", number.Int64())
	result.ID = newID("otp_")
	result.ExpiresAt = now.Add(5 * time.Minute)
	rendered := Rendered{Subject: "Your verification code", HTML: "<p>Your verification code is <strong>" + code + "</strong>.</p><p>This code expires in five minutes. Do not share it.</p>", Plain: "Your verification code is " + code + ".\nThis code expires in five minutes. Do not share it."}
	// Revoke only the same intent. Different purposes retain independent codes,
	// but share the recipient cooldown and the application's normal quotas.
	rows, err := tx.Query(ctx, `UPDATE otp_challenges SET revoked_at=$4,code_hash=''::bytea WHERE application_id=$1 AND lower(recipient)=lower($2) AND purpose=$3 AND consumed_at IS NULL AND revoked_at IS NULL RETURNING message_id`, app, in.To, in.Purpose, now)
	if err != nil {
		return result, err
	}
	var oldMessages []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return result, err
		}
		oldMessages = append(oldMessages, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	if err = retireOTPMessages(ctx, tx, oldMessages); err != nil {
		return result, err
	}
	inMail := SendInput{ApplicationID: app, To: in.To, Class: "otp", ExpiresAt: &result.ExpiresAt}
	result.MessageID, _, err = a.submitWith(ctx, tx, inMail, "edp:otp:"+result.ID, &rendered)
	if err != nil {
		return OTPChallenge{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO otp_challenges(id,application_id,recipient,purpose,code_hash,created_at,expires_at,message_id,idempotency_key,payload_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, result.ID, app, in.To, in.Purpose, otpDigest(a.C.Secret, result.ID, app, in.To, in.Purpose, code), now, result.ExpiresAt, result.MessageID, key, fingerprint)
	if err != nil {
		return OTPChallenge{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return OTPChallenge{}, err
	}
	return result, nil
}

// A worker claims message before application. Do not wait for a claimed message
// while holding application or challenge locks: dispatch checks revocation too.
func retireOTPMessages(ctx context.Context, tx pgx.Tx, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `WITH locked AS (SELECT id,status FROM messages WHERE id=ANY($1) AND otp_encrypted FOR UPDATE SKIP LOCKED), changed AS (UPDATE messages m SET html='',plain='',status=CASE WHEN l.status IN('queued','retry_wait') THEN 'cancelled' ELSE m.status END,updated_at=now() FROM locked l WHERE m.id=l.id RETURNING m.id,m.status,l.status AS old_status) INSERT INTO delivery_events(message_id,status,detail) SELECT id,status,'OTP challenge retired' FROM changed WHERE old_status IN('queued','retry_wait')`, ids)
	return err
}

type OTPVerification struct {
	Verified bool   `json:"verified"`
	To       string `json:"to,omitempty"`
	Purpose  string `json:"purpose,omitempty"`
}

func (a *App) verifyOTP(ctx context.Context, app, id, purpose, code string) (OTPVerification, error) {
	var result OTPVerification
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	var recipient, intent, messageID string
	var digest []byte
	var attempts int
	var expires time.Time
	var consumed, revoked *time.Time
	err = tx.QueryRow(ctx, "SELECT recipient,purpose,message_id,code_hash,attempts,expires_at,consumed_at,revoked_at FROM otp_challenges WHERE id=$1 AND application_id=$2 FOR UPDATE", id, app).Scan(&recipient, &intent, &messageID, &digest, &attempts, &expires, &consumed, &revoked)
	if err == pgx.ErrNoRows {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return result, err
	}
	if consumed != nil || revoked != nil || attempts >= otpAttempts || !expires.After(now) || len(digest) == 0 {
		return result, nil
	}
	if purpose == intent && hmac.Equal(digest, otpDigest(a.C.Secret, id, app, recipient, intent, code)) {
		_, err = tx.Exec(ctx, "UPDATE otp_challenges SET consumed_at=$2,code_hash=''::bytea WHERE id=$1", id, now)
		result = OTPVerification{Verified: true, To: recipient, Purpose: intent}
	} else {
		_, err = tx.Exec(ctx, "UPDATE otp_challenges SET attempts=attempts+1,code_hash=CASE WHEN attempts+1 >= $2 THEN ''::bytea ELSE code_hash END WHERE id=$1", id, otpAttempts)
	}
	if err != nil {
		return OTPVerification{}, err
	}
	if result.Verified || attempts+1 >= otpAttempts {
		if err = retireOTPMessages(ctx, tx, []string{messageID}); err != nil {
			return OTPVerification{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return OTPVerification{}, err
	}
	return result, nil
}

func (a *App) apiOTPCreate(w http.ResponseWriter, r *http.Request) {
	app, ok := a.apiScope(w, r, "otp.create")
	if !ok {
		return
	}
	var in OTPInput
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if !decodeAPI(w, r, &in) {
		return
	}
	result, err := a.createOTP(r.Context(), app, in, r.Header.Get("Idempotency-Key"))
	if err != nil {
		otpProblem(w, err)
		return
	}
	if !result.Replayed {
		a.audit(r.Context(), person(r).KeyID, "otp.create", app, result.ID)
	}
	respond(w, 202, result)
}

func (a *App) apiOTPVerify(w http.ResponseWriter, r *http.Request) {
	app, ok := a.apiScope(w, r, "otp.verify")
	if !ok {
		return
	}
	var in struct {
		Purpose string `json:"purpose"`
		Code    string `json:"code"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if !decodeAPI(w, r, &in) {
		return
	}
	if in.Purpose == "" || in.Code == "" || len(in.Purpose) > 64 || len(in.Code) > 64 {
		problem(w, 400, "purpose and code required (maximum 64 characters each)")
		return
	}
	result, err := a.verifyOTP(r.Context(), app, r.PathValue("id"), in.Purpose, in.Code)
	if err != nil {
		otpProblem(w, err)
		return
	}
	if result.Verified {
		a.audit(r.Context(), person(r).KeyID, "otp.verified", app, r.PathValue("id"))
	}
	respond(w, 200, result)
}

func otpProblem(w http.ResponseWriter, err error) {
	var problemErr *HTTPError
	if errors.As(err, &problemErr) {
		if problemErr.Code == 429 {
			w.Header().Set("Retry-After", strconv.Itoa(60))
		}
		problem(w, problemErr.Code, problemErr.Message)
	} else {
		problem(w, 500, "OTP operation could not be persisted")
	}
}
