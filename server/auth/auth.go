package auth

import (
	"time"

	"github.com/tinode/chat/server/store/types"
)

// Level is the type for authentication levels.
type Level int

// Authentication levels
const (
	// LevelNone is undefined/not authenticated
	LevelNone Level = iota * 10
	// LevelAnon is anonymous user/light authentication
	LevelAnon
	// LevelAuth is fully authenticated user
	LevelAuth
	// LevelRoot is a superuser (currently unused)
	LevelRoot
)

// String implements Stringer interface: gets human-readable name for a numeric authentication level.
func (a Level) String() string {
	switch a {
	case LevelNone:
		return ""
	case LevelAnon:
		return "anon"
	case LevelAuth:
		return "auth"
	case LevelRoot:
		return "root"
	default:
		return "unkn"
	}
}

// ParseAuthLevel parses authentication level from a string.
func ParseAuthLevel(name string) Level {
	switch name {
	case "anon":
		return LevelAnon
	case "auth":
		return LevelAuth
	case "root":
		return LevelRoot
	default:
		return LevelNone
	}
}

// Feature is a bitmap of authenticated features, such as validated/not validated.
type Feature uint16

const (
	// Validated bit is set if user's credentials are already validated.
	Validated Feature = 1 << iota
)

// Rec is an authentication record.
type Rec struct {
	// User ID
	Uid types.Uid
	// Authentication level
	AuthLevel Level
	// Lifetime of this record
	Lifetime time.Duration
	// Bitmap of features. Currently 'validated'/'not validated' only.
	Features Feature
	// Tags generated by this authentication record.
	Tags []string
}

// AuthHandler is the interface which auth providers must implement.
type AuthHandler interface {
	// Init initialize the handler.
	Init(jsonconf string) error

	// AddRecord adds persistent record to database.
	// Returns: updated auth record, error
	AddRecord(rec *Rec, secret []byte) (*Rec, error)

	// UpdateRecord updates existing record with new credentials. Returns a numeric error code to indicate
	// if the error is due to a duplicate or some other error.
	UpdateRecord(rec *Rec, secret []byte) error

	// Authenticate: given a user-provided authentication secret (such as "login:password"
	// return user ID, time when the secret expires (zero, if never) or an error code.
	// store.Users.GetAuthRecord("scheme", "unique")
	// Returns: user auth record, error.
	Authenticate(secret []byte) (*Rec, error)

	// IsUnique verifies if the provided secret can be considered unique by the auth scheme
	// E.g. if login is unique.
	IsUnique(secret []byte) (bool, error)

	// GenSecret generates a new secret, if appropriate.
	GenSecret(rec *Rec) ([]byte, time.Time, error)

	// DelRecords deletes all authentication records for the given user.
	DelRecords(uid types.Uid) error
}
