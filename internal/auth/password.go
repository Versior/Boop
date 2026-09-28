package auth

import (
	"errors"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost is bcrypt.DefaultCost: at ~50ms per hash it is the usual balance
// between brute-force cost and single-core response latency.
const bcryptCost = bcrypt.DefaultCost

// dummyPasswordHash is a real bcrypt hash of an unrelated random string. Login
// verifies against it when the email is unknown so a failed sign-in costs the
// same time whether or not the account exists.
const dummyPasswordHash = "$2a$10$H/9LOY4cK6vq2oY4qOX3Qe/UiBc1.cVRRLqj/IU9IY95aLzMmdJcO"

// ValidatePassword enforces the documented 10–72 byte rule. The upper bound is
// bcrypt's: it ignores input beyond 72 bytes.
func ValidatePassword(password string) error {
	if len(password) < MinPasswordBytes || len(password) > MaxPasswordBytes {
		return ErrInvalidPassword
	}
	return nil
}

// HashPassword validates and hashes a password with bcrypt.
func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", errors.New("auth: hash password: " + err.Error())
	}
	return string(hash), nil
}

// VerifyPassword compares a password with a stored hash. Any malformed or
// empty hash verifies nothing.
func VerifyPassword(hash, password string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// VerifyDummyPassword burns the same bcrypt cost as a real verification, for
// login attempts on unknown accounts.
func VerifyDummyPassword(password string) {
	_ = bcrypt.CompareHashAndPassword([]byte(dummyPasswordHash), []byte(password))
}
