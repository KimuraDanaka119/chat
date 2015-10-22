# Tinode Instant Messaging Server

**This documentation covers the next 0.4 release of Tinode. ETA mid-November 2015.**  

Instant messaging server. Backend in pure [Go](http://golang.org) ([Affero GPL 3.0](http://www.gnu.org/licenses/agpl-3.0.en.html)), client-side binding in Java for Android and Javascript ([Apache 2.0](http://www.apache.org/licenses/LICENSE-2.0)), persistent storage [RethinkDB](http://rethinkdb.com/), JSON over websocket (long polling is also available). No UI components other than demo apps. Tinode is meant as a replacement for XMPP.

This is alpha-quality software. Bugs should be expected. Version 0.4

## Why?

[XMPP](http://xmpp.org/) is a mature specification with support for a very broad spectrum of use cases developed long before mobile became important. As a result most (all?) known XMPP servers are difficult to adapt for the most common use case of a few people messaging each other from mobile devices. Tinode is an attempt to build a modern replacement for XMPP/Jabber focused on a narrow use case of instant messaging between humans with emphasis on mobile communication.

## Features

### Supported

* One on one messaging
* Group chats:
 * Groups (topics) with up to 32 members where every member's access permissions are managed individually
 * Groups with unlimited number of members with bearer token access control
* Topic access control with separate permissions for various actions (reading, writing, sharing, etc)
* Server-generated presence notifications for people and topics
* Persistent message store
* Android Java bindings (dependencies: [jackson](https://github.com/FasterXML/jackson), [android-websockets](https://github.com/codebutler/android-websockets))
* Javascript bindings with no dependencies
* Websocket & long polling transport
* JSON wire protocol
* Server-generated message delivery status
* Support for client-side content caching
* Blocking users on the server

### Planned

* iOS client bindings
* Mobile push notification hooks
* Clustering
* Federation
* Multitenancy
* Different levels of message persistence (from strict persistence to store until delivered to purely ephemeral messaging)
* Support for binary wire protocol
* User search/discovery
* Anonymous clients
* Support for other SQL and NoSQL backends
* Pluggable authentication

## How it works?

Tinode is an IM router and a store. Conceptually it loosely follows a publish-subscribe model.

Server connects sessions, users, and topics. Session is a network connection between a client application and the server. User represents a human being who connects to the server with a session. Topic is a named communication channel which routes content between sessions.

Users and topics are assigned unique IDs. User ID is a string with 'usr' prefix followed by base64-URL-encoded pseudo-random 64-bit number, e.g. `usr2il9suCbuko`. Topic IDs are described below.

Clients such as mobile or web applications create sessions by connecting to the server over a websocket or through long polling. Client authentication is optional (*anonymous clients are technically supported but may not fully work as expected yet*). Client authenticates the session by sending a `{login}` packet. Only basic authentication with user name and password is currently supported. Multiple simultaneous sessions may be established by the same user. Logging out is not supported.

Once the session is established, the user can start interacting with other users through topics. The following
topic types are available:

* `me` is a topic for managing one's profile, receiving invites and requests for approval; 'me' topic exists for every user.
* Peer to peer topic is a communication channel strictly between two users. It's named as a 'p2p' prefix followed by a base64-URL-encoded numeric part of user IDs concatenated in ascending order, e.g. `p2p2il9suCbukqm4P2KFOv9-w`. Peer to peer topics must be explicitly created.
* Group topic is a channel for multi-user communication. It's named as 'grp' followed by 12 pseudo-random characters, i.e. `grp1XUtEhjv6HND`. Group topics must be explicitly created.

Session joins a topic by sending a `{sub}` packet. Packet `{sub}` serves three functions: creating a new topic, subscribing user to a topic, and attaching session to a topic. See {sub} section below for details.

Once the session has joined the topic, the user may start generating content by sending `{pub}` packets. The content is delivered to other attached sessions as `{data}` packets.

The user may query or update topic metadata by sending `{get}` and `{set}` packets.

Changes to topic metadata, such as changes in topic description, or when other users join or leave the topic, is reported to live sessions with `{pres}` (presence) packet.

When user's `me` topic comes online (i.e. an authenticated session attaches to `me` topic), a `{pres}` packet is sent to `me` topics of all other users, who have peer to peer subscriptions with the first user.

## Connecting to the server

Client establishes a connection to the server over HTTP. Server offers two end points:
 * `/v0/channels` for websocket connections
 * `/v0/v0/channels/lp` for long polling

`v0` denotes API version (currently zero). Every HTTP request must include API key in the request. It may be included in the URL as `...?apikey=<YOUR_API_KEY>`, in the request body, or in an HTTP header `X-Tinode-APIKey`.

Server responds to connection with a `{ctrl}` message. The `params` field contains protocol version:
`"params":{"ver":"0.4"}`  


### Websocket

Messages are sent in text frames. Binary frames are reserved for future use.

### Long polling



## Messages

A message is a logically associated set of data. Messages are passed as JSON-formatted text.

All client to server messages may have an optional `id` field. It's set by the client as means to receive an aknowledgement from the server that the message was received and processed. The `id` is expected to be a session-unique string but it can be any string. The server does not attempt to interpret it other than to check JSON validity. It's returned unchanged by the server when it replies to client messages.

For brievity the notation below omits double quotes around field names as well as outer curly brackets.

For messages that update application-defined data, such as `{set}` `private` or `public` fields, in case the server-side
data needs to be cleared, use a string with a single Unicode DEL character "&#x2421;" `"\u2421"`.  

### Client to server messages

#### `{acc}`

Message `{acc}` is used for creating users or updating authentication credentials. To create a new user set
`acc.user` to string "new". Either authenticated or anonymous session can send an `{acc}` message to create a new user.
To update credentials leave `acc.user` unset.

```js
acc: {
  id: "1a2b3",     // string, client-provided message id, optional
  user: "new", // string, "new" to create a new user, default: current user, optional
  auth: [   // array of authentication schemes to add, update or delete
    {
      scheme: "basic", // requested authentication scheme for this account, required;
                       // only "basic" (default) is currently supported. The current
                       // basic scheme does not allow changes to username.
      secret: "username:password" // string, secret for the chosen authentication
                      // scheme; to delete a scheme use string with a single DEL
                      // Unicode character "\u2421"; required
    }
  ],
  init: {  // object, user initialization data closely matching that of table
           // initialization; optional
    defacs: {
      auth: "RWS", // string, default access mode for peer to peer conversations
                   // between this user and other authenticated users
      anon: "X"  // string, default access mode for peer to peer conversations between
                 // this user and anonymous (un-authenticated) users
    }, // Default access mode for user's peer to peer topics
    public: {}, // Free-form application-dependent payload to describe user,
                // available to everyone
    private: {} // Private application-dependent payload available to user only
                // through `me` topic
  }
}
```

Server responds with a `{ctrl}` message with `ctrl.params` containing details of the new user. If `init.acs` is missing,
server will assign server-default access values.

#### `{login}`

Login is used to authenticate the current session.

```js
login: {
  id: "1a2b3",     // string, client-provided message id, optional
  scheme: "basic", // string, authentication scheme, optional; only "basic" (default)
                   // is currently supported
  secret: "username:password", // string, secret for the chosen authentication scheme,
                               // required
  expireIn: "24h", // string, login expiration time in Go's time.ParseDuration format,
                   // see below, optional
  tag: "some string" // string, client instance ID; tag is used to support caching,
                     // optional
}
```
Basic authentication scheme expects `secret` to be a string composed of a user name followed by a colon `:` followed by a plan text password.

[time.ParseDuration](https://golang.org/pkg/time/#ParseDuration) is used to parse `expireIn`. The recognized format is a possibly signed sequence of decimal numbers, each with optional fraction and a unit suffix, such as "300ms", "-1.5h" or "2h45m". Valid time units are "ns", "us" (or "µs"), "ms", "s", "m", "h".

Server responds to a `{login}` packet with a `{ctrl}` packet.

#### `{sub}`

The `{sub}` packet serves three functions:
 * creating a topic
 * subscribing user to a topic
 * attaching session to a topic

User creates a new group topic by sending `{sub}` packet with the `topic` field set to `new`. Server will create a topic and respond back to session with the name of the newly created topic.

User creates a new peer to peer topic by sending `{sub}` packet with `topic` set to peer's user ID.  

The user is always subscribed to and the sessions is attached to the newly created topic.

If the user had no relationship with the topic, sending `{sub}` packet creates it. Subscribing means to establish a relationship between session's user and the topic when no relationship existed in the past.

Joining (attaching to) a topic means for the session to start consuming content from the topic. Server automatically differentiates between subscribing and joining/attaching based on context: if the user had no prior relationship with the topic, the server subscribes the user then attaches the current session to the topic. If relationship existed, the server only attaches the session to the topic. When subscribing, the server checks user's access permissions against topic's access control list. It may grant immediate access, deny access, may generate a request for approval from topic managers.

Server replies to the `{sub}` with a `{ctrl}`.

The `{sub}` message may include a `get` and `browse` fields which mirror `what` and `browse` fields of a {get} message. If included, server will treat them as a subsequent `{get}` message on the same topic. In that case the reply may also include `{meta}` and `{data}` messages.


```js
sub: {
  id: "1a2b3",  // string, client-provided message id, optional
  topic: "me",   // topic to be subscribed or attached to

  // Object with topic initialization data, new topics & new
  // subscriptions only, mirrors {set info}
  init: {
    defacs: {
      auth: "RWS", // string, default access for new authenticated subscribers
      anon: "X"    // string, default access for new anonymous (un-authenticated)
                   // subscribers
    }, // Default access mode for the new topic
    public: {}, // Free-form application-dependent payload to describe topic
    private: {} // Per-subscription private application-dependent payload
  }, // object, optional

  // Subscription parameters, mirrors {set sub}; sub.user must
  // not be provided
  sub: {
    mode: "RWS", // string, requested access mode, optional; default: server-defined
    info: {}    // a free-form payload to pass to the topic manager
  }, // object, optional

  // Metadata to request from the topic; space-separated list, valid strings
  // are "info", "sub", "data"; default: request nothing; unknown strings are
  // ignored; see {get  what} for details
  get: "info sub data", // string, optional

  // Optional parameters for get: "info", see {get what="data"} for details
  browse: {
    ascnd: true, // boolean, sort in ascending order by time, otherwise
                 // descending (default), optional
    since: "2015-09-06T18:07:30.134Z", // datetime as RFC 3339-formated string,
                    // load objects newer than this (inclusive/closed), optional
    before: "2015-10-06T18:07:30.134Z", // datetime as  RFC 3339-formated string,
                    // load objects older than this (exclusive/open), optional
    limit: 20, // integer, limit the number of returned objects, default: 32, optional
  } // object, optional
}
```

#### `{leave}`

This is a counterpart to `{sub}` message. It also serves two functions:
* leaving the topic without unsubscribing (`unsub=false`)
* unsubscribing (`unsub=true`)

Server responds to `{leave}` with a `{ctrl}` packet. Leaving without unsubscribing affects just the current session. Leaving with unsubscribing will affect all user's sessions.

```js
leave: {
  id: "1a2b3",  // string, client-provided message id, optional
  topic: "grp1XUtEhjv6HND",   // string, topic to leave, unsubscribe, or
                              // delete, required
  unsub: true // boolean, leave and unsubscribe, optional, default: false
```

#### `{pub}`

The message is used to distribute content to topic subscribers.

```js
pub: {
  id: "1a2b3", // string, client-provided message id, optional
  topic: "grp1XUtEhjv6HND", // topic to publish to, required
  content: {}  // object, free-form content to publish to topic
               // subscribers, required
}
```

Topic subscribers receive the content as `{data}` message.

#### `{get}`

Query topic for metadata, such as description or a list of subscribers, or query message history.

```js
get: {
  what: "sub info data", // string, space-separated list of parameters to query;
                        // unknown strings are ignored; required
  browse: {
    ascnd: true, // boolean, sort in ascending order by time, otherwise
                 // descending (default), optional
    since: "2015-09-06T18:07:30.134Z", // datetime as RFC 3339-formated string,
                        // load objects newer than this (inclusive/closed), optional
    before: "2015-10-06T18:07:30.134Z", // datetime as  RFC 3339-formated string,
                        // load objects older than this (exclusive/open), optional
    limit: 20, // integer, limit the number of returned objects, default: 32,
               // optional
  } // object, what=data query parameters
}
```

* `{get what="info"}`

Query topic description. Server responds with a `{meta}` message containing requested data. See `{meta}` for details.

* `{get what="sub"}`

Get a list of subscribers. Server responds with a `{meta}` message containing a list of subscribers. See `{meta}` for details.
For `me` topic the request returns a list of user's subscriptions.

* `{get what="data"}`

Query message history. Server sends `{data}` messages matching parameters provided in the `browse` field of the query.
The `id` field of the data messages is not provided as it's common for data messages.


#### `{set}`

Update topic metadata, delete messages or topic.

```js
set: {
  id: "1a2b3", // string, client-provided message id, optional
  topic: "grp1XUtEhjv6HND", // string, topic to publish to, required
  what: "sub info", // string, space separated list of data to update,
                        // unknown strings are ignored
  info: {}, // object, payload for what == "info"
  sub: {} // object, payload for what == "sub"
}
```

#### `{del}`

Delete messages or topic.

```js
del: {
  id: "1a2b3", // string, client-provided message id, optional
  topic: "grp1XUtEhjv6HND", // string, topic affect, required
  what: "msg", // string, either "topic" or "msg" (default); what to delete - the
               // entire topic or just the messages, optional, default: "msg"
  hard: false, // boolean, request to delete messages for all users, default: false
  before: "2015-10-06T18:07:30.134Z", // datetime as a RFC 3339-
              // formated string, delete messages older than this
              // (exclusive of the value itself), optional
}
```

No special permission is needed to soft-delete messages `hard=false`. Soft-deleting messages hide them from the
requesting user. `D` permission is needed to hard-delete messages. Only owner can delete a topic.

### Server to client messages

Messages to a session generated in response to a specific request contain an `id` field equal to the id of the
originating message. The `id` is not interpreted by the server.

Most server to client messages have a `ts` field which is a timestamp when the message was generated by the server. Timestamp is in [RFC 3339](https://tools.ietf.org/html/rfc3339) format, timezone is always UTC, precision to milliseconds.

#### `{data}`

Content published in the topic. These messages are the only messages persisted in database; `{data}` messages are
broadcast to all topic subscribers with an `R` permission.

```js
data: {
  topic: "grp1XUtEhjv6HND", // string, topic which distributed this message,
                            // always present
  from: "usr2il9suCbuko", // string, id of the user who published the
                          // message; could be missing if the message was
                          // generated by the server
  ts: "2015-10-06T18:07:30.038Z", // string, timestamp
  content: {} // object, content exactly as published by the user in the
              // {pub} message
}
```

#### `{ctrl}`

Generic response indicating an error or a success condition. The message is sent to the originating session.

```js
ctrl: {
  id: "1a2b3", // string, client-provided message id, optional
  topic: "grp1XUtEhjv6HND", // string, topic name, if this is a response in context
                            // of a topic, optional
  code: 200, // integer, code indicating success or failure of the request, follows
             // the HTTP status codes model, always present
  text: "OK", // string, text with more details about the result, always present
  params: {}, // object, generic response parameters, context-dependent, optional
  ts: "2015-10-06T18:07:30.038Z", // string, timestamp
}
```

#### `{meta}`

Information about topic metadata or subscribers, sent in response to `{set}` or `{sub}` message to the originating session.

```js
ctrl: {
  id: "1a2b3", // string, client-provided message id, optional
  topic: "grp1XUtEhjv6HND", // string, topic name, if this is a response in
                            // context of a topic, optional
	info: {

  }, // object, topic description, optional
	sub:  [

  ] // array of objects, topic subscribers, optional
  ts: "2015-10-06T18:07:30.038Z", // string, timestamp
}
```

#### `{pres}`

Notification that topic metadata has changed. Timestamp is not present.

```js
pres: {
  topic: "grp1XUtEhjv6HND", // string, topic affected by the change, always present
  user: "usr2il9suCbuko", // user affected by the change, present if relevant
  what: ""  // string, what's changed, always present
}
```

## Access control

Access control manages user's access to topics through access control lists (ACLs) or bearer tokens (not implemented as of version 0.4).

User's access to a topic is defined by two sets of permissions: user's desired permissions, and topic's given permissions. Each permission is a bit in a bitmap. It can be either present or absent. The actual access is determined as a bitwise AND of wanted and given permissions. The permissions are represented as a set of symbols, where presence of a symbol means a set permission bit:

* No access: `N` is not a permission per se but an indicator that permissions are explicitly cleared/not set. It usually means that the default permissions should not be applied.
* Read: `R`, permission to receive `{data}` packets
* Write: `W`, permission to `{pub}` to topic
* Presence: `P`, permission to receive presence updates `{pres}`
* Sharing: `S`, permission to invite other people to join a topic and to approve requests to join
* Delete: `D`, permission to hard-delete messages; only owners can completely delete topics
* Owner: `O`, user is the topic owner
* Banned: `X`, user has no access, requests to share/gain access/`{sub}` are silently ignored

Topic's default access is established at the topic creation time by `{sub.init.acs}` field and can be subsequently modified by `{set}` messages. Default access is applied to all new subscriptions. It can be assigned on a per-user basis by `{set}` messages.

## Topics

Topic is a named communication channel for one or more people. All timestamps are represented as RFC 3999-formatted string with precision to milliseconds and timezone always set to UTC, e.g. `"2015-10-06T18:07:29.841Z"`.

### `me` topic

Topic `me` is automatically created for every user at the account creation time. It serves as means for account updates, receiving presence notification from people and topics of interest, invites to join topics, requests to approve subscription for topics where this user is a manager (has `S` permission). Topic `me` cannot be deleted or unsubscribed from. One can leave the topic which will stop all relevant communication and indicate that the user is offline (although the user may still be logged in and may continue to use other topics).

Joining or leaving `me` generates a `{pres}` presence update sent to all users who have peer to peer topics with the given user with appropriate permissions.  

Topic `me` is read-only. `{pub}` messages to `me` are rejected.

The `{data}` message represents invites and requests to confirm a subscription. The `from` field of the message contains ID of the user who originated the request, for instance, the user who asked current user to join a topic or the user who requested an approval for subscription. The `content` field of the message contains the following information:
* act: request action as string; possible actions are:
 * "info" to notify the user that user's request to subscribe was approved; in case of peer to peer topics this could be a notification that the peer has subscribed to the topic
 * "join" is an invitation to subscribe to a topic
 * "appr" is a request to approve a subscription
* topic: the name of the topic, in case of an invite the current user is invited to this topic; in case of a request to approve, another user wants to subscribe to this topic where the current user is a manager (has `S` permission)
* user: user ID as a string of the user who is the target of this request. In case of an invite this is the ID of the current user; in case of an approval request this is the ID of the user who is being subscribed.
* acs: object describing access permissions of the subscription, see [Access control][] for details
* info: object with a free-form payload. It's passed unchanged from the originating `{sub}` or `{set}` request.

Message `{get what="info"}` to `me` is automatically replied with a `{meta}` message containing `info` section with the following information:
* created: timestamp of topic creation time
* updated: timestamp of when topic's `public` or `private` was last updated
* acs: object describing user's access permissions, `{"want":"RDO","given":"RDO"}`, see [Access control][] for details
* lastMsg: timestamp when last `{data}` message was sent through the topic
* seen: an object describing when the topic was last accessed by the current user from any client instance. This should be used if the client implements data caching. See [Support for Client-Side Caching] for more details.
 * when": timestamp of the last access
 * tag: string provided by the client instance when it accessed the topic.
* seenTag: timestamp when the topic was last accessed from a session with the current client instance. See [Support for Client-Side Caching] for more details
* public: an object with application-defined content which describes the user, such user name "Jane Doe" or any other information which is made freely available to other users.
* private: an object with application-defined content which is made available only to user's own sessions.

Message `{get what="sub"}` to `me` is different from any other topic as it returns the list of topics that the current user is subscribed to as opposite to the user's subscription to `me`.

Message `{get what="data"}` to `me` queries the history of invites/notifications. It's handled the same way as to any other topic.

### Peer to Peer Topics `p2pAAABBB`


### Group Topics `grpABCDEFG`


## Support for Client-Side Caching
