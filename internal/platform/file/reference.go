package file

import (
	"encoding/base64"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

const referencePrefix = "/files/"

var (
	ErrInvalidReference = errors.New("invalid managed file reference")
	managedKeyPattern   = regexp.MustCompile(`^users/([1-9][0-9]*)/(image|file)/[0-9]{8}/[0-9a-f]{32}\.[a-z0-9]+$`)
)

type managedReference struct {
	Key     string
	OwnerID int64
	Kind    string
}

func referenceForKey(key string) string {
	return referencePrefix + base64.RawURLEncoding.EncodeToString([]byte(key))
}

func parseReference(reference string) (managedReference, error) {
	if !strings.HasPrefix(reference, referencePrefix) {
		return managedReference{}, ErrInvalidReference
	}
	token := strings.TrimPrefix(reference, referencePrefix)
	if token == "" || strings.Contains(token, "/") {
		return managedReference{}, ErrInvalidReference
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != token {
		return managedReference{}, ErrInvalidReference
	}
	key := string(decoded)
	parts := managedKeyPattern.FindStringSubmatch(key)
	if len(parts) != 3 {
		return managedReference{}, ErrInvalidReference
	}
	ownerID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || ownerID <= 0 {
		return managedReference{}, ErrInvalidReference
	}
	return managedReference{Key: key, OwnerID: ownerID, Kind: parts[2]}, nil
}
