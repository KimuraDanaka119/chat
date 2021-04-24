package main

import (
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/tinode/chat/server/auth"
	"github.com/tinode/chat/server/auth/mock_auth"
	"github.com/tinode/chat/server/store"
	"github.com/tinode/chat/server/store/mock_store"
	"github.com/tinode/chat/server/store/types"
)

func TestDispatchHello(t *testing.T) {
	s := &Session{
		send:    make(chan interface{}, 10),
		uid:     types.Uid(1),
		authLvl: auth.LevelAuth,
	}
	wg := sync.WaitGroup{}
	r := Responses{}
	wg.Add(1)
	go s.testWriteLoop(&r, &wg)
	msg := &ClientComMessage{
		Hi: &MsgClientHi{
			Id:        "123",
			Version:   "1",
			UserAgent: "test-ua",
			Lang:      "en-GB",
		},
	}
	s.dispatch(msg)
	close(s.send)
	wg.Wait()
	if len(r.messages) != 1 {
		t.Errorf("Responses: expected 1, received %d.", len(r.messages))
	}
	resp := r.messages[0].(*ServerComMessage)
	if resp == nil {
		t.Fatal("Response must be ServerComMessage")
	}
	if resp.Ctrl != nil {
		if resp.Ctrl.Code != 201 {
			t.Errorf("Response code: expected 201, got %d", resp.Ctrl.Code)
		}
		if resp.Ctrl.Params == nil {
			t.Error("Response is expected to contain params dict.")
		}
	} else {
		t.Error("Response must contain a ctrl message.")
	}

	if s.lang != "en-GB" {
		t.Errorf("Session language expected to be 'en-GB' vs '%s'", s.lang)
	}
	if s.userAgent != "test-ua" {
		t.Errorf("Session UA expected to be 'test-ua' vs '%s'", s.userAgent)
	}
	if s.countryCode != "GB" {
		t.Errorf("Country code expected to be 'GB' vs '%s'", s.countryCode)
	}
	if s.ver == 0 {
		t.Errorf("s.ver expected 0 vs found %d", s.ver)
	}
}

func TestDispatchLogin(t *testing.T) {
	ctrl := gomock.NewController(t)
	ss := mock_store.NewMockStoreInterface(ctrl)
	aa := mock_auth.NewMockAuthHandler(ctrl)

	uid := types.Uid(1)
	store.Store = ss
	defer func() {
		store.Store = nil
		ctrl.Finish()
	}()

	secret := "<==auth-secret==>"
	authRec := &auth.Rec{
		Uid:       uid,
		AuthLevel: auth.LevelAuth,
		Tags:      []string{"tag1", "tag2"},
		State:     types.StateOK,
	}
	ss.EXPECT().GetLogicalAuthHandler("basic").Return(aa)
	aa.EXPECT().Authenticate([]byte(secret), gomock.Any()).Return(authRec, nil, nil)
	// Token generation.
	ss.EXPECT().GetLogicalAuthHandler("token").Return(aa)
	token := "<==auth-token==>"
	expires, _ := time.Parse(time.RFC822, "01 Jan 50 00:00 UTC")
	aa.EXPECT().GenSecret(authRec).Return([]byte(token), expires, nil)

	s := &Session{
		send:    make(chan interface{}, 10),
		authLvl: auth.LevelAuth,
		ver:     16,
	}
	wg := sync.WaitGroup{}
	r := Responses{}
	wg.Add(1)
	go s.testWriteLoop(&r, &wg)

	msg := &ClientComMessage{
		Login: &MsgClientLogin{
			Id:     "123",
			Scheme: "basic",
			Secret: []byte(secret),
		},
	}

	s.dispatch(msg)
	close(s.send)
	wg.Wait()

	if len(r.messages) != 1 {
		t.Errorf("Responses: expected 1, received %d.", len(r.messages))
	}
	resp := r.messages[0].(*ServerComMessage)
	if resp == nil {
		t.Fatal("Response must be ServerComMessage")
	}
	if resp.Ctrl != nil {
		if resp.Ctrl.Id != "123" {
			t.Errorf("Response id: expected '123', found '%s'", resp.Ctrl.Id)
		}
		if resp.Ctrl.Code != 200 {
			t.Errorf("Response code: expected 200, got %d", resp.Ctrl.Code)
		}
		if resp.Ctrl.Params == nil {
			t.Error("Response is expected to contain params dict.")
		}
		p := resp.Ctrl.Params.(map[string]interface{})
		if authToken := string(p["token"].([]byte)); authToken != token {
			t.Errorf("Auth token: expected '%s', found '%s'.", token, authToken)
		}
		if exp := p["expires"].(time.Time); exp != expires {
			t.Errorf("Token expiration: expected '%s', found '%s'.", expires, exp)
		}
	} else {
		t.Error("Response must contain a ctrl message.")
	}
}

func TestDispatchSubscribe(t *testing.T) {
	uid := types.Uid(1)
	s := &Session{
		send:         make(chan interface{}, 10),
		uid:          uid,
		authLvl:      auth.LevelAuth,
		inflightReqs: &sync.WaitGroup{},
		ver:          15,
	}
	wg := sync.WaitGroup{}
	r := Responses{}
	wg.Add(1)
	go s.testWriteLoop(&r, &wg)

	hub := &Hub{
		join: make(chan *sessionJoin, 10),
	}
	globals.hub = hub

	defer func() {
		globals.hub = nil
	}()

	msg := &ClientComMessage{
		Sub: &MsgClientSub{
			Id:    "123",
			Topic: "me",
			Get: &MsgGetQuery{
				What: "sub desc tags cred",
			},
		},
	}

	s.dispatch(msg)
	close(s.send)
	wg.Wait()

	// Check we've routed the join request via the hub.
	if len(r.messages) != 0 {
		t.Errorf("Responses: expected 0, received %d.", len(r.messages))
	}
	if len(hub.join) == 1 {
		join := <-hub.join
		if join.sess != s {
			t.Error("Hub.join request: sess field expected to be the session under test.")
		}
		if join.pkt != msg {
			t.Error("Hub.join request: subscribe message expected to be the original subscribe message.")
		}
	} else {
		t.Errorf("Hub join messages: expected 1, received %d.", len(hub.join))
	}
	s.inflightReqs.Done()
}

func TestDispatchLeave(t *testing.T) {
	uid := types.Uid(1)
	s := &Session{
		send:         make(chan interface{}, 10),
		uid:          uid,
		authLvl:      auth.LevelAuth,
		inflightReqs: &sync.WaitGroup{},
		ver:          15,
	}
	wg := sync.WaitGroup{}
	r := Responses{}
	wg.Add(1)
	go s.testWriteLoop(&r, &wg)

	destUid := types.Uid(2)
	topicName := uid.P2PName(destUid)
	leave := make(chan *sessionLeave, 1)
	s.subs = make(map[string]*Subscription)
	s.subs[topicName] = &Subscription{
		done: leave,
	}

	msg := &ClientComMessage{
		Leave: &MsgClientLeave{
			Id:    "123",
			Topic: destUid.UserId(),
		},
	}

	s.dispatch(msg)
	close(s.send)
	wg.Wait()

	// Check we've routed the join request via the leave channel.
	if len(r.messages) != 0 {
		t.Errorf("Responses: expected 0, received %d.", len(r.messages))
	}
	if len(leave) == 1 {
		req := <-leave
		if req.sess != s {
			t.Error("Leave request: sess field expected to be the session under test.")
		}
		if req.pkt != msg {
			t.Error("Leave request: leave message expected to be the original leave message.")
		}
	} else {
		t.Errorf("Unsub messages: expected 1, received %d.", len(leave))
	}
	if len(s.subs) != 0 {
		t.Errorf("Session subs: expected to be empty, actual size: %d", len(s.subs))
	}
	s.inflightReqs.Done()
}

func TestDispatchPublish(t *testing.T) {
	uid := types.Uid(1)
	s := &Session{
		send:         make(chan interface{}, 10),
		uid:          uid,
		authLvl:      auth.LevelAuth,
		inflightReqs: &sync.WaitGroup{},
		ver:          15,
	}
	wg := sync.WaitGroup{}
	r := Responses{}
	wg.Add(1)
	go s.testWriteLoop(&r, &wg)

	destUid := types.Uid(2)
	topicName := uid.P2PName(destUid)

	brdcst := make(chan *ServerComMessage, 1)
	s.subs = make(map[string]*Subscription)
	s.subs[topicName] = &Subscription{
		broadcast: brdcst,
	}

	testMessage := "test content"
	msg := &ClientComMessage{
		Pub: &MsgClientPub{
			Id:      "123",
			Topic:   destUid.UserId(),
			Content: testMessage,
		},
	}

	s.dispatch(msg)
	close(s.send)
	wg.Wait()

	// Check we've routed the join request via the broadcast channel.
	if len(r.messages) != 0 {
		t.Errorf("Responses: expected 0, received %d.", len(r.messages))
	}
	if len(brdcst) == 1 {
		req := <-brdcst
		if req.sess != s {
			t.Error("Pub request: sess field expected to be the session under test.")
		}
		if req.Data.Content != testMessage {
			t.Errorf("Pub request content: expected '%s' vs '%s'.", testMessage, req.Data.Content)
		}
		if req.Data.Topic != destUid.UserId() {
			t.Errorf("Pub request topic: expected '%s' vs '%s'.", destUid.UserId(), req.Data.Topic)
		}
		if req.Data.From != uid.UserId() {
			t.Errorf("Pub request from: expected '%s' vs '%s'.", uid.UserId(), req.Data.From)
		}
	} else {
		t.Errorf("Pub messages: expected 1, received %d.", len(brdcst))
	}
}

func TestDispatchGet(t *testing.T) {
	uid := types.Uid(1)
	s := &Session{
		send:         make(chan interface{}, 10),
		uid:          uid,
		authLvl:      auth.LevelAuth,
		inflightReqs: &sync.WaitGroup{},
		ver:          15,
	}
	wg := sync.WaitGroup{}
	r := Responses{}
	wg.Add(1)
	go s.testWriteLoop(&r, &wg)

	destUid := types.Uid(2)
	topicName := uid.P2PName(destUid)

	meta := make(chan *metaReq, 1)
	s.subs = make(map[string]*Subscription)
	s.subs[topicName] = &Subscription{
		meta: meta,
	}

	msg := &ClientComMessage{
		Get: &MsgClientGet{
			Id:    "123",
			Topic: destUid.UserId(),
			MsgGetQuery: MsgGetQuery{
				What: "desc sub del cred",
			},
		},
	}

	s.dispatch(msg)
	close(s.send)
	wg.Wait()

	// Check we've routed the join request via the meta channel.
	if len(r.messages) != 0 {
		t.Errorf("Responses: expected 0, received %d.", len(r.messages))
	}
	if len(meta) == 1 {
		req := <-meta
		if req.sess != s {
			t.Error("Get request: sess field expected to be the session under test.")
		}
		if req.pkt != msg {
			t.Error("Get request pkt: expected original client request.")
		}
	} else {
		t.Errorf("Get messages: expected 1, received %d.", len(meta))
	}
}

func TestDispatchSet(t *testing.T) {
	uid := types.Uid(1)
	s := &Session{
		send:         make(chan interface{}, 10),
		uid:          uid,
		authLvl:      auth.LevelAuth,
		inflightReqs: &sync.WaitGroup{},
		ver:          15,
	}
	wg := sync.WaitGroup{}
	r := Responses{}
	wg.Add(1)
	go s.testWriteLoop(&r, &wg)

	destUid := types.Uid(2)
	topicName := uid.P2PName(destUid)

	meta := make(chan *metaReq, 1)
	s.subs = make(map[string]*Subscription)
	s.subs[topicName] = &Subscription{
		meta: meta,
	}

	msg := &ClientComMessage{
		Set: &MsgClientSet{
			Id:    "123",
			Topic: destUid.UserId(),
			MsgSetQuery: MsgSetQuery{
				Desc: &MsgSetDesc{},
				Sub:  &MsgSetSub{},
				Tags: []string{"abc"},
				Cred: &MsgCredClient{},
			},
		},
	}

	s.dispatch(msg)
	close(s.send)
	wg.Wait()

	// Check we've routed the join request via the meta channel.
	if len(r.messages) != 0 {
		t.Errorf("Responses: expected 0, received %d.", len(r.messages))
	}
	if len(meta) == 1 {
		req := <-meta
		if req.sess != s {
			t.Error("Set request: sess field expected to be the session under test.")
		}
		if req.pkt != msg {
			t.Error("Set request pkt: expected original client request.")
		}
		expectedWhat := constMsgMetaDesc | constMsgMetaSub | constMsgMetaTags | constMsgMetaCred
		if req.pkt.MetaWhat != expectedWhat {
			t.Errorf("Set request what: expected %d vs %d", expectedWhat, req.pkt.MetaWhat)
		}
	} else {
		t.Errorf("Set messages: expected 1, received %d.", len(meta))
	}
}

func TestDispatchDelMsg(t *testing.T) {
	uid := types.Uid(1)
	s := &Session{
		send:         make(chan interface{}, 10),
		uid:          uid,
		authLvl:      auth.LevelAuth,
		inflightReqs: &sync.WaitGroup{},
		ver:          15,
	}
	wg := sync.WaitGroup{}
	r := Responses{}
	wg.Add(1)
	go s.testWriteLoop(&r, &wg)

	destUid := types.Uid(2)
	topicName := uid.P2PName(destUid)

	meta := make(chan *metaReq, 1)
	s.subs = make(map[string]*Subscription)
	s.subs[topicName] = &Subscription{
		meta: meta,
	}

	msg := &ClientComMessage{
		Del: &MsgClientDel{
			Id:     "123",
			Topic:  destUid.UserId(),
			What:   "msg",
			DelSeq: []MsgDelRange{MsgDelRange{LowId: 3, HiId: 4}},
			Hard:   true,
		},
	}

	s.dispatch(msg)
	close(s.send)
	wg.Wait()

	// Check we've routed the join request via the meta channel.
	if len(r.messages) != 0 {
		t.Errorf("Responses: expected 0, received %d.", len(r.messages))
	}
	if len(meta) == 1 {
		req := <-meta
		if req.sess != s {
			t.Error("Del request: sess field expected to be the session under test.")
		}
		if req.pkt != msg {
			t.Error("Del request pkt: expected original client request.")
		}
	} else {
		t.Errorf("Del messages: expected 1, received %d.", len(meta))
	}
}

func TestDispatchNote(t *testing.T) {
	uid := types.Uid(1)
	s := &Session{
		send:         make(chan interface{}, 10),
		uid:          uid,
		authLvl:      auth.LevelAuth,
		inflightReqs: &sync.WaitGroup{},
		ver:          15,
	}
	wg := sync.WaitGroup{}
	r := Responses{}
	wg.Add(1)
	go s.testWriteLoop(&r, &wg)

	destUid := types.Uid(2)
	topicName := uid.P2PName(destUid)

	brdcst := make(chan *ServerComMessage, 1)
	s.subs = make(map[string]*Subscription)
	s.subs[topicName] = &Subscription{
		broadcast: brdcst,
	}

	msg := &ClientComMessage{
		Note: &MsgClientNote{
			Topic: destUid.UserId(),
			What:  "recv",
			SeqId: 5,
		},
	}

	s.dispatch(msg)
	close(s.send)
	wg.Wait()

	// Check we've routed the join request via the broadcast channel.
	if len(r.messages) != 0 {
		t.Errorf("Responses: expected 0, received %d.", len(r.messages))
	}
	if len(brdcst) == 1 {
		req := <-brdcst
		if req.sess != s {
			t.Error("Pub request: sess field expected to be the session under test.")
		}
		if req.Info.What != msg.Note.What {
			t.Errorf("Note request what: expected '%s' vs '%s'.", msg.Note.What, req.Info.What)
		}
		if req.Info.SeqId != msg.Note.SeqId {
			t.Errorf("Note request seqId: expected %d vs %d.", msg.Note.SeqId, req.Info.SeqId)
		}
		if req.Info.Topic != destUid.UserId() {
			t.Errorf("Note request topic: expected '%s' vs '%s'.", destUid.UserId(), req.Info.Topic)
		}
		if req.Info.From != uid.UserId() {
			t.Errorf("Note request from: expected '%s' vs '%s'.", uid.UserId(), req.Info.From)
		}
		if req.SkipSid != s.sid {
			t.Errorf("Note request skipSid: expected '%s' vs '%s'.", s.sid, req.SkipSid)
		}
	} else {
		t.Errorf("Note messages: expected 1, received %d.", len(brdcst))
	}
}
