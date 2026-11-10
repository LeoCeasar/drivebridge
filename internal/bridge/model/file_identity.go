package model

import (
	"errors"
	"regexp"
	"time"
)

type FileIdentityKind string

const (
	FileIdentityServerVersion FileIdentityKind = "server_version"
	FileIdentityObservation   FileIdentityKind = "observed_download"
)

type FileIdentity struct {
	Kind          FileIdentityKind `json:"kind"`
	Version       string           `json:"version,omitempty"`
	ObservationID string           `json:"observation_id,omitempty"`
	SHA256        string           `json:"sha256"`
	StartedAt     time.Time        `json:"started_at"`
	CompletedAt   time.Time        `json:"completed_at"`
}

var fileSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)
var observationUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func (f FileIdentity) Validate() error {
	if !fileSHA.MatchString(f.SHA256) || f.StartedAt.IsZero() || f.CompletedAt.IsZero() || f.CompletedAt.Before(f.StartedAt) {
		return errors.New("invalid file identity")
	}
	switch f.Kind {
	case FileIdentityObservation:
		if f.Version != "" || !observationUUID.MatchString(f.ObservationID) {
			return errors.New("invalid observed download identity")
		}
	case FileIdentityServerVersion:
		if f.Version == "" || f.ObservationID != "" {
			return errors.New("invalid server version identity")
		}
	default:
		return errors.New("invalid file identity kind")
	}
	return nil
}
