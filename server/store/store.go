package store

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/tinode/chat/server/auth"
	"github.com/tinode/chat/server/db"
	"github.com/tinode/chat/server/media"
	"github.com/tinode/chat/server/store/types"
	"github.com/tinode/chat/server/validate"
)

var adp adapter.Adapter
var mediaHandler media.Handler

// Unique ID generator
var uGen types.UidGenerator

type configType struct {
	// 16-byte key for XTEA. Used to initialize types.UidGenerator
	UidKey []byte `json:"uid_key"`
	// Configurations for individual adapters.
	Adapters map[string]json.RawMessage `json:"adapters"`
}

func openAdapter(workerId int, jsonconf string) error {
	var config configType
	if err := json.Unmarshal([]byte(jsonconf), &config); err != nil {
		return errors.New("store: failed to parse config: " + err.Error() + "(" + jsonconf + ")")
	}

	if adp == nil {
		return errors.New("store: database adapter is missing")
	}

	if adp.IsOpen() {
		return errors.New("store: connection is already opened")
	}

	// Initialise snowflake
	if workerId < 0 || workerId > 1023 {
		return errors.New("store: invalid worker ID")
	}

	if err := uGen.Init(uint(workerId), config.UidKey); err != nil {
		return errors.New("store: failed to init snowflake: " + err.Error())
	}

	var adapterConfig string
	if config.Adapters != nil {
		adapterConfig = string(config.Adapters[adp.GetName()])
	}

	return adp.Open(adapterConfig)
}

// Open initializes the persistence system. Adapter holds a connection pool for a database instance.
// 	 name - name of the adapter rquested in the config file
//   jsonconf - configuration string
func Open(workerId int, jsonconf string) error {
	if err := openAdapter(workerId, jsonconf); err != nil {
		return err
	}

	return adp.CheckDbVersion()
}

// Close terminates connection to persistent storage.
func Close() error {
	if adp.IsOpen() {
		return adp.Close()
	}

	return nil
}

// IsOpen checks if persistent storage connection has been initialized.
func IsOpen() bool {
	if adp != nil {
		return adp.IsOpen()
	}

	return false
}

// GetAdapterName returns the name of the current adater.
func GetAdapterName() string {
	if adp != nil {
		return adp.GetName()
	}

	return ""
}

// InitDb creates a new database instance. If 'reset' is true it will first attempt to drop
// existing database. If jsconf is nil it will assume that the connection is already open.
// If it's non-nil, it will use the config string to open the DB connection first.
func InitDb(jsonconf string, reset bool) error {
	if !IsOpen() {
		if err := openAdapter(1, jsonconf); err != nil {
			return err
		}
	}
	return adp.CreateDb(reset)
}

// RegisterAdapter makes a persistence adapter available.
// If Register is called twice or if the adapter is nil, it panics.
func RegisterAdapter(name string, a adapter.Adapter) {
	if a == nil {
		panic("store: Register adapter is nil")
	}

	if adp != nil {
		panic("store: adapter '" + adp.GetName() + "' is already registered")
	}

	adp = a
}

// GetUid generates a unique ID suitable for use as a primary key.
func GetUid() types.Uid {
	return uGen.Get()
}

// GetUidString generate unique ID as string
func GetUidString() string {
	return uGen.GetStr()
}

// DecodeUid takes an XTEA encrypted Uid and decrypts it into an int64.
// This is needed for sql compatibility. Tte original int64 values
// are generated by snowflake which ensures that the top bit is unset.
func DecodeUid(uid types.Uid) int64 {
	if uid.IsZero() {
		return 0
	}
	return uGen.DecodeUid(uid)
}

// EncodeUid applies XTEA encryption to an int64 value. It's the inverse of DecodeUid.
func EncodeUid(id int64) types.Uid {
	if id == 0 {
		return types.ZeroUid
	}
	return uGen.EncodeInt64(id)
}

// UsersObjMapper is a users struct to hold methods for persistence mapping for the User object.
type UsersObjMapper struct{}

// Users is the ancor for storing/retrieving User objects
var Users UsersObjMapper

// Create inserts User object into a database, updates creation time and assigns UID
func (UsersObjMapper) Create(user *types.User, private interface{}) (*types.User, error) {

	user.SetUid(GetUid())
	user.InitTimes()

	err := adp.UserCreate(user)
	if err != nil {
		return nil, err
	}

	// Create user's subscription to 'me' && 'find'. These topics are ephemeral, the topic object need not to be
	// inserted.
	err = Subs.Create(
		&types.Subscription{
			ObjHeader: types.ObjHeader{CreatedAt: user.CreatedAt},
			User:      user.Id,
			Topic:     user.Uid().UserId(),
			ModeWant:  types.ModeCSelf,
			ModeGiven: types.ModeCSelf,
			Private:   private,
		},
		&types.Subscription{
			ObjHeader: types.ObjHeader{CreatedAt: user.CreatedAt},
			User:      user.Id,
			Topic:     user.Uid().FndName(),
			ModeWant:  types.ModeCSelf,
			ModeGiven: types.ModeCSelf,
			Private:   nil,
		})
	if err != nil {
		// Best effort to delete incomplete user record. Orphaned user records are not a problem.
		// They just take up space.
		adp.UserDelete(user.Uid(), true)
		return nil, err
	}

	return user, nil
}

// GetAuthRecord takes a user ID and a authentication scheme name, fetches unique scheme-dependent identifier and
// authentication secret.
func (UsersObjMapper) GetAuthRecord(user types.Uid, scheme string) (string, auth.Level, []byte, time.Time, error) {
	unique, authLvl, secret, expires, err := adp.AuthGetRecord(user, scheme)
	if err == nil {
		parts := strings.Split(unique, ":")
		unique = parts[1]
	}
	return unique, authLvl, secret, expires, err
}

// GetAuthUniqueRecord takes a unique identifier and a authentication scheme name, fetches user ID and
// authentication secret.
func (UsersObjMapper) GetAuthUniqueRecord(scheme, unique string) (types.Uid, auth.Level, []byte, time.Time, error) {
	return adp.AuthGetUniqueRecord(scheme + ":" + unique)
}

// AddAuthRecord creates a new authentication record for the given user.
func (UsersObjMapper) AddAuthRecord(uid types.Uid, authLvl auth.Level, scheme, unique string, secret []byte,
	expires time.Time) (bool, error) {

	return adp.AuthAddRecord(uid, scheme, scheme+":"+unique, authLvl, secret, expires)
}

// UpdateAuthRecord updates authentication record with a new secret and expiration time.
func (UsersObjMapper) UpdateAuthRecord(uid types.Uid, authLvl auth.Level, scheme, unique string,
	secret []byte, expires time.Time) (bool, error) {

	return adp.AuthUpdRecord(uid, scheme, scheme+":"+unique, authLvl, secret, expires)
}

// DelAuthRecords deletes user's auth records of the given scheme.
func (UsersObjMapper) DelAuthRecords(uid types.Uid, scheme string) error {
	return adp.AuthDelScheme(uid, scheme)
}

// Get returns a user object for the given user id
func (UsersObjMapper) Get(uid types.Uid) (*types.User, error) {
	return adp.UserGet(uid)
}

// GetAll returns a slice of user objects for the given user ids
func (UsersObjMapper) GetAll(uid ...types.Uid) ([]types.User, error) {
	return adp.UserGetAll(uid...)
}

// GetByCred returns user ID for the given validated credential.
func (UsersObjMapper) GetByCred(method, value string) (types.Uid, error) {
	return adp.UserGetByCred(method, value)
}

// Delete deletes user records.
func (UsersObjMapper) Delete(id types.Uid, hard bool) error {
	return adp.UserDelete(id, hard)
}

// GetDisabled returns user IDs which were disabled (soft-deleted) since specifid time.
func (UsersObjMapper) GetDisabled(since time.Time) ([]types.Uid, error) {
	return adp.UserGetDisabled(since)
}

// UpdateLastSeen updates LastSeen and UserAgent.
func (UsersObjMapper) UpdateLastSeen(uid types.Uid, userAgent string, when time.Time) error {
	return adp.UserUpdate(uid, map[string]interface{}{"LastSeen": when, "UserAgent": userAgent})
}

// Update is a generic user data update.
func (UsersObjMapper) Update(uid types.Uid, update map[string]interface{}) error {
	update["UpdatedAt"] = types.TimeNow()
	return adp.UserUpdate(uid, update)
}

// UpdateTags either resets tags to the given slice or creates a union of existing tags and the new ones.
func (UsersObjMapper) UpdateTags(uid types.Uid, tags []string, reset bool) error {
	return adp.UserUpdateTags(uid, tags, reset)
}

// GetSubs loads a list of subscriptions for the given user. Does not load Public, does not load
// deleted subscriptions.
func (UsersObjMapper) GetSubs(id types.Uid, opts *types.QueryOpt) ([]types.Subscription, error) {
	return adp.SubsForUser(id, false, opts)
}

// FindSubs find a list of users and topics for the given tags. Results are formatted as subscriptions.
func (UsersObjMapper) FindSubs(id types.Uid, required, optional []string) ([]types.Subscription, error) {
	usubs, err := adp.FindUsers(id, required, optional)
	if err != nil {
		return nil, err
	}
	tsubs, err := adp.FindTopics(required, optional)
	if err != nil {
		return nil, err
	}
	return append(usubs, tsubs...), nil
}

// GetTopics load a list of user's subscriptions with Public field copied to subscription
func (UsersObjMapper) GetTopics(id types.Uid, opts *types.QueryOpt) ([]types.Subscription, error) {
	return adp.TopicsForUser(id, false, opts)
}

// GetTopicsAny load a list of user's subscriptions with Public field copied to subscription.
// Deleted topics are returned too.
func (UsersObjMapper) GetTopicsAny(id types.Uid, opts *types.QueryOpt) ([]types.Subscription, error) {
	return adp.TopicsForUser(id, true, opts)
}

// GetOwnTopics retuens a slice of group topic names where the user is the owner.
func (UsersObjMapper) GetOwnTopics(id types.Uid, opts *types.QueryOpt) ([]string, error) {
	return adp.OwnTopics(id, opts)
}

// SaveCred saves a credential validation request.
func (UsersObjMapper) SaveCred(cred *types.Credential) error {
	cred.InitTimes()
	return adp.CredAdd(cred)
}

// ConfirmCred marks credential as confirmed.
func (UsersObjMapper) ConfirmCred(id types.Uid, method string) error {
	return adp.CredConfirm(id, method)
}

// FailCred increments fail count.
func (UsersObjMapper) FailCred(id types.Uid, method string) error {
	return adp.CredFail(id, method)
}

// GetCred gets a list of confirmed credentials.
func (UsersObjMapper) GetCred(id types.Uid, method string) (*types.Credential, error) {
	var creds []*types.Credential
	var err error
	if creds, err = adp.CredGet(id, method); err == nil {
		if len(creds) > 0 {
			return creds[0], nil
		}
		return nil, nil
	}
	return nil, err

}

// GetAllCred retrieves all confimed credential for the given user.
func (UsersObjMapper) GetAllCred(id types.Uid) ([]*types.Credential, error) {
	return adp.CredGet(id, "")
}

// DelCred deletes user's credentials. If method is "" all credentials are deleted.
func (UsersObjMapper) DelCred(id types.Uid, method string) error {
	return adp.CredDel(id, method)
}

// GetUnreadCount returs user's total count of unread messages in all topics with the R permissions
func (UsersObjMapper) GetUnreadCount(id types.Uid) (int, error) {
	return adp.UserUnreadCount(id)
}

// TopicsObjMapper is a struct to hold methods for persistence mapping for the topic object.
type TopicsObjMapper struct{}

// Topics is an instance of TopicsObjMapper to map methods to.
var Topics TopicsObjMapper

// Create creates a topic and owner's subscription to it.
func (TopicsObjMapper) Create(topic *types.Topic, owner types.Uid, private interface{}) error {

	topic.InitTimes()
	topic.TouchedAt = &topic.CreatedAt
	topic.Owner = owner.String()

	err := adp.TopicCreate(topic)
	if err != nil {
		return err
	}

	if !owner.IsZero() {
		err = Subs.Create(&types.Subscription{
			ObjHeader: types.ObjHeader{CreatedAt: topic.CreatedAt},
			User:      owner.String(),
			Topic:     topic.Id,
			ModeGiven: types.ModeCFull,
			ModeWant:  topic.GetAccess(owner),
			Private:   private})
	}

	return err
}

// CreateP2P creates a P2P topic by generating two user's subsciptions to each other.
func (TopicsObjMapper) CreateP2P(initiator, invited *types.Subscription) error {
	initiator.InitTimes()
	initiator.SetTouchedAt(&initiator.CreatedAt)
	invited.InitTimes()
	invited.SetTouchedAt((&invited.CreatedAt))

	return adp.TopicCreateP2P(initiator, invited)
}

// Get a single topic with a list of relevant users de-normalized into it
func (TopicsObjMapper) Get(topic string) (*types.Topic, error) {
	return adp.TopicGet(topic)
}

// GetUsers loads subscriptions for topic plus loads user.Public.
// Deleted subscriptions are not loaded.
func (TopicsObjMapper) GetUsers(topic string, opts *types.QueryOpt) ([]types.Subscription, error) {
	return adp.UsersForTopic(topic, false, opts)
}

// GetUsersAny loads subscriptions for topic plus loads user.Public. It's the same as GetUsers,
// except it loads deleted subscriptions too.
func (TopicsObjMapper) GetUsersAny(topic string, opts *types.QueryOpt) ([]types.Subscription, error) {
	return adp.UsersForTopic(topic, true, opts)
}

// GetSubs loads a list of subscriptions to the given topic, user.Public and deleted
// subscriptions are not loaded
func (TopicsObjMapper) GetSubs(topic string, opts *types.QueryOpt) ([]types.Subscription, error) {
	return adp.SubsForTopic(topic, false, opts)
}

// GetSubsAny loads a list of subscriptions to the given topic including deleted subscription.
// user.Public is not loaded
func (TopicsObjMapper) GetSubsAny(topic string, opts *types.QueryOpt) ([]types.Subscription, error) {
	return adp.SubsForTopic(topic, true, opts)
}

// Update is a generic topic update.
func (TopicsObjMapper) Update(topic string, update map[string]interface{}) error {
	update["UpdatedAt"] = types.TimeNow()
	return adp.TopicUpdate(topic, update)
}

// OwnerChange replaces the old topic owner with the new owner.
func (TopicsObjMapper) OwnerChange(topic string, newOwner, oldOwner types.Uid) error {
	return adp.TopicOwnerChange(topic, newOwner, oldOwner)
}

// Delete deletes topic, messages, attachments, and subscriptions.
func (TopicsObjMapper) Delete(topic string, hard bool) error {
	return adp.TopicDelete(topic, hard)
}

// SubsObjMapper is A struct to hold methods for persistence mapping for the Subscription object.
type SubsObjMapper struct{}

// Subs is an instance of SubsObjMapper to map methods to.
var Subs SubsObjMapper

// Create creates multiple subscriptions
func (SubsObjMapper) Create(subs ...*types.Subscription) error {
	for _, sub := range subs {
		sub.InitTimes()
	}

	_, err := adp.TopicShare(subs)
	return err
}

// Get given subscription
func (SubsObjMapper) Get(topic string, user types.Uid) (*types.Subscription, error) {
	return adp.SubscriptionGet(topic, user)
}

// Update values of topic's subscriptions.
func (SubsObjMapper) Update(topic string, user types.Uid, update map[string]interface{}, updateTS bool) error {
	if updateTS {
		update["UpdatedAt"] = types.TimeNow()
	}
	return adp.SubsUpdate(topic, user, update)
}

// Delete deletes a subscription
func (SubsObjMapper) Delete(topic string, user types.Uid) error {
	return adp.SubsDelete(topic, user)
}

// MessagesObjMapper is a struct to hold methods for persistence mapping for the Message object.
type MessagesObjMapper struct{}

// Messages is an instance of MessagesObjMapper to map methods to.
var Messages MessagesObjMapper

// Save message
func (MessagesObjMapper) Save(msg *types.Message, readBySender bool) error {
	msg.InitTimes()

	// Increment topic's or user's SeqId
	err := adp.TopicUpdateOnMessage(msg.Topic, msg)
	if err != nil {
		return err
	}

	// Check if the message has attachments. If so, link earlier uploaded files to message.
	var attachments []string
	if header, ok := msg.Head["attachments"]; ok {
		// The header is typed as []interface{}, convert to []string
		if arr, ok := header.([]interface{}); ok {
			for _, val := range arr {
				if url, ok := val.(string); ok {
					// Convert attachment URLs to file IDs.
					if fid := mediaHandler.GetIdFromUrl(url); !fid.IsZero() {
						attachments = append(attachments, fid.String())
					}
				}
			}
		}

		if len(attachments) == 0 {
			delete(msg.Head, "attachments")
		}
	}

	err = adp.MessageSave(msg)
	if err != nil {
		return err
	}

	// Mark message as read by the sender.
	if readBySender {
		// Ignore the error here. It's not a big deal if it fails.
		adp.SubsUpdate(msg.Topic, types.ParseUid(msg.From),
			map[string]interface{}{
				"RecvSeqId": msg.SeqId,
				"ReadSeqId": msg.SeqId})
	}

	if len(attachments) > 0 {
		return adp.MessageAttachments(msg.Uid(), attachments)
	}

	return nil
}

// DeleteList deletes multiple messages defined by a list of ranges.
func (MessagesObjMapper) DeleteList(topic string, delID int, forUser types.Uid, ranges []types.Range) error {
	var toDel *types.DelMessage
	if delID > 0 {
		toDel = &types.DelMessage{
			Topic:       topic,
			DelId:       delID,
			DeletedFor:  forUser.String(),
			SeqIdRanges: ranges}
		toDel.InitTimes()
	}

	err := adp.MessageDeleteList(topic, toDel)
	if err != nil {
		return err
	}

	// TODO: move to adapter
	if delID > 0 {
		// Record ID of the delete transaction
		err = adp.TopicUpdate(topic, map[string]interface{}{"DelId": delID})
		if err != nil {
			return err
		}

		// Soft-deleting will update one subscription, hard-deleting will ipdate all.
		// Soft- or hard- is defined by the forUser being defined.
		err = adp.SubsUpdate(topic, forUser, map[string]interface{}{"DelId": delID})
		if err != nil {
			return err
		}
	}

	return err
}

// GetAll returns multiple messages.
func (MessagesObjMapper) GetAll(topic string, forUser types.Uid, opt *types.QueryOpt) ([]types.Message, error) {
	return adp.MessageGetAll(topic, forUser, opt)
}

// GetDeleted returns the ranges of deleted messages and the largest DelId reported in the list.
func (MessagesObjMapper) GetDeleted(topic string, forUser types.Uid, opt *types.QueryOpt) ([]types.Range, int, error) {
	dmsgs, err := adp.MessageGetDeleted(topic, forUser, opt)
	if err != nil {
		return nil, 0, err
	}

	var ranges []types.Range
	var maxID int
	// Flatten out the ranges
	for i := range dmsgs {
		dm := &dmsgs[i]
		if dm.DelId > maxID {
			maxID = dm.DelId
		}
		ranges = append(ranges, dm.SeqIdRanges...)
	}
	sort.Sort(types.RangeSorter(ranges))
	ranges = types.RangeSorter(ranges).Normalize()

	return ranges, maxID, nil
}

// Registered authentication handlers.
var authHandlers map[string]auth.AuthHandler

// Logical auth handler names
var authHandlerNames map[string]string

// RegisterAuthScheme registers an authentication scheme handler.
// The 'name' must be the hardcoded name, NOT the logical name.
func RegisterAuthScheme(name string, handler auth.AuthHandler) {
	if name == "" {
		panic("RegisterAuthScheme: empty auth scheme name")
	}
	if handler == nil {
		panic("RegisterAuthScheme: scheme handler is nil")
	}

	name = strings.ToLower(name)
	if authHandlers == nil {
		authHandlers = make(map[string]auth.AuthHandler)
	}
	if _, dup := authHandlers[name]; dup {
		panic("RegisterAuthScheme: called twice for scheme " + name)
	}
	authHandlers[name] = handler
}

// GetAuthNames returns all addressable auth handler names, logical and hardcoded
// excluding those which are disabled like "basic:".
func GetAuthNames() []string {
	if len(authHandlers) == 0 {
		return nil
	}

	var allNames []string
	for name := range authHandlers {
		allNames = append(allNames, name)
	}
	for name := range authHandlerNames {
		allNames = append(allNames, name)
	}

	var names []string
	for _, name := range allNames {
		if GetLogicalAuthHandler(name) != nil {
			names = append(names, name)
		}
	}

	return names

}

// GetAuthHandler returns an auth handler by actual hardcoded name irrspectful of logical naming.
func GetAuthHandler(name string) auth.AuthHandler {
	return authHandlers[strings.ToLower(name)]
}

// GetLogicalAuthHandler returns an auth handler by logical name. If there is no handler by that
// logical name it tries to find one by the hardcoded name.
func GetLogicalAuthHandler(name string) auth.AuthHandler {
	name = strings.ToLower(name)
	if len(authHandlerNames) != 0 {
		if lname, ok := authHandlerNames[name]; ok {
			return authHandlers[lname]
		}
	}
	return authHandlers[name]
}

// InitAuthLogicalNames initializes authentication mapping "logical handler name":"actual handler name".
// Logical name must not be empty, actual name could be an empty string.
func InitAuthLogicalNames(config json.RawMessage) error {
	if config == nil || string(config) == "null" {
		return nil
	}
	var mapping []string
	if err := json.Unmarshal(config, &mapping); err != nil {
		return errors.New("store: failed to parse logical auth names: " + err.Error() + "(" + string(config) + ")")
	}
	if len(mapping) == 0 {
		return nil
	}

	if authHandlerNames == nil {
		authHandlerNames = make(map[string]string)
	}
	for _, pair := range mapping {
		if parts := strings.Split(pair, ":"); len(parts) == 2 {
			if parts[0] == "" {
				return errors.New("store: empty logical auth name '" + pair + "'")
			}
			parts[0] = strings.ToLower(parts[0])
			if _, ok := authHandlerNames[parts[0]]; ok {
				return errors.New("store: duplicate mapping for logical auth name '" + pair + "'")
			}
			parts[1] = strings.ToLower(parts[1])
			if parts[1] != "" {
				if _, ok := authHandlers[parts[1]]; !ok {
					return errors.New("store: unknown handler for logical auth name '" + pair + "'")
				}
			}
			if parts[0] == parts[1] {
				// Skip useless identity mapping.
				continue
			}
			authHandlerNames[parts[0]] = parts[1]
		} else {
			return errors.New("store: invalid logical auth mapping '" + pair + "'")
		}
	}
	return nil
}

// Registered authentication handlers.
var validators map[string]validate.Validator

// RegisterValidator registers validation scheme.
func RegisterValidator(name string, v validate.Validator) {
	name = strings.ToLower(name)
	if validators == nil {
		validators = make(map[string]validate.Validator)
	}

	if v == nil {
		panic("RegisterValidator: validator is nil")
	}
	if _, dup := validators[name]; dup {
		panic("RegisterValidator: called twice for validator " + name)
	}
	validators[name] = v
}

// GetValidator returns registered validator by name.
func GetValidator(name string) validate.Validator {
	return validators[strings.ToLower(name)]
}

// DeviceMapper is a struct to map methods used for handling device IDs, used to generate push notifications.
type DeviceMapper struct{}

// Devices is an instance of DeviceMapper to map methods to.
var Devices DeviceMapper

// Update updates a device record.
func (DeviceMapper) Update(uid types.Uid, oldDeviceID string, dev *types.DeviceDef) error {
	// If the old device Id is specified and it's different from the new ID, delete the old id
	if oldDeviceID != "" && (dev == nil || dev.DeviceId != oldDeviceID) {
		if err := adp.DeviceDelete(uid, oldDeviceID); err != nil {
			return err
		}
	}

	// Insert or update the new DeviceId if one is given.
	if dev != nil && dev.DeviceId != "" {
		return adp.DeviceUpsert(uid, dev)
	}
	return nil
}

// GetAll returns all known device IDS for a given list of user IDs.
func (DeviceMapper) GetAll(uid ...types.Uid) (map[types.Uid][]types.DeviceDef, int, error) {
	return adp.DeviceGetAll(uid...)
}

// Delete deletes device record for a given user.
func (DeviceMapper) Delete(uid types.Uid, deviceID string) error {
	return adp.DeviceDelete(uid, deviceID)
}

// Registered media/file handlers.
var fileHandlers map[string]media.Handler

// RegisterMediaHandler saves reference to a media handler (file upload-download handler).
func RegisterMediaHandler(name string, mh media.Handler) {
	if fileHandlers == nil {
		fileHandlers = make(map[string]media.Handler)
	}

	if mh == nil {
		panic("RegisterMediaHandler: handler is nil")
	}
	if _, dup := fileHandlers[name]; dup {
		panic("RegisterMediaHandler: called twice for handler " + name)
	}
	fileHandlers[name] = mh
}

// GetMediaHandler returns default media handler.
func GetMediaHandler() media.Handler {
	return mediaHandler
}

// UseMediaHandler sets specified media handler as default.
func UseMediaHandler(name, config string) error {
	mediaHandler = fileHandlers[name]
	if mediaHandler == nil {
		panic("UseMediaHandler: unknown handler '" + name + "'")
	}
	return mediaHandler.Init(config)
}

// FileMapper is a struct to map methods used for file handling.
type FileMapper struct{}

// Files is an instance of FileMapper to be used for handling file uploads.
var Files FileMapper

// StartUpload records that the given user initiated a file upload
func (FileMapper) StartUpload(fd *types.FileDef) error {
	fd.Status = types.UploadStarted
	return adp.FileStartUpload(fd)
}

// FinishUpload marks started upload as successfully finished.
func (FileMapper) FinishUpload(fid string, success bool, size int64) (*types.FileDef, error) {
	status := types.UploadCompleted
	if !success {
		status = types.UploadFailed
	}
	return adp.FileFinishUpload(fid, status, size)
}

// Get fetches a file record for a unique file id.
func (FileMapper) Get(fid string) (*types.FileDef, error) {
	return adp.FileGet(fid)
}

// DeleteUnused removes unused attachments.
func (FileMapper) DeleteUnused(olderThan time.Time, limit int) error {
	toDel, err := adp.FileDeleteUnused(olderThan, limit)
	if err != nil {
		return err
	}
	if len(toDel) > 0 {
		return GetMediaHandler().Delete(toDel)
	}
	return nil
}
