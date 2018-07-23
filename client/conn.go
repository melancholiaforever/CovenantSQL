/*
 * Copyright 2018 The ThunderDB Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package client

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	bp "gitlab.com/thunderdb/ThunderDB/blockproducer"
	"gitlab.com/thunderdb/ThunderDB/crypto/asymmetric"
	"gitlab.com/thunderdb/ThunderDB/crypto/kms"
	"gitlab.com/thunderdb/ThunderDB/kayak"
	"gitlab.com/thunderdb/ThunderDB/proto"
	"gitlab.com/thunderdb/ThunderDB/rpc"
	"gitlab.com/thunderdb/ThunderDB/sqlchain/storage"
	"gitlab.com/thunderdb/ThunderDB/utils/log"
	wt "gitlab.com/thunderdb/ThunderDB/worker/types"
)

// conn implements an interface sql.Conn.
type conn struct {
	dbID         proto.DatabaseID
	connectionID uint64
	seqNo        uint64

	queries   []storage.Query
	peers     *kayak.Peers
	peersLock sync.RWMutex
	nodeID    proto.NodeID
	privKey   *asymmetric.PrivateKey
	pubKey    *asymmetric.PublicKey

	inTransaction bool
	closed        int32
	closeCh       chan struct{}
}

func newConn(cfg *Config) (c *conn, err error) {
	if cfg.Debug {
		log.SetLevel(log.DebugLevel)
	}

	// generate random connection id
	connID := rand.Int63()
	if connID < 0 {
		connID = -connID
	}

	// get local node id
	var nodeID proto.NodeID
	if nodeID, err = kms.GetLocalNodeID(); err != nil {
		return
	}

	// get local private key
	var privKey *asymmetric.PrivateKey
	if privKey, err = kms.GetLocalPrivateKey(); err != nil {
		return
	}

	// get local public key
	var pubKey *asymmetric.PublicKey
	if pubKey, err = kms.GetLocalPublicKey(); err != nil {
		return
	}

	c = &conn{
		dbID:         proto.DatabaseID(cfg.DatabaseID),
		connectionID: uint64(connID),
		nodeID:       nodeID,
		privKey:      privKey,
		pubKey:       pubKey,
		queries:      make([]storage.Query, 0),
		closeCh:      make(chan struct{}),
	}

	c.log("new conn database ", c.dbID)

	// get peers from BP
	if err = c.getPeers(); err != nil {
		return
	}

	// start peers update routine
	go func() {
		ticker := time.NewTicker(cfg.PeersUpdateInterval)
		defer ticker.Stop()

		for {
			select {
			case <-c.closeCh:
				return
			case <-ticker.C:
			}

			if err = c.getPeers(); err != nil {
				c.log("update peers failed ", err.Error())
			}
		}
	}()

	return
}

func (c *conn) log(msg ...interface{}) {
	log.Println(msg...)
}

// Prepare implements the driver.Conn.Prepare method.
func (c *conn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

// Close implements the driver.Conn.Close method.
func (c *conn) Close() error {
	// close the meta connection
	if atomic.CompareAndSwapInt32(&c.closed, 0, 1) {
		c.log("closed connection")
	}

	select {
	case <-c.closeCh:
	default:
		close(c.closeCh)
	}

	return nil
}

// Begin implements the driver.Conn.Begin method.
func (c *conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

// BeginTx implements the driver.ConnBeginTx.BeginTx method.
func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if atomic.LoadInt32(&c.closed) != 0 {
		return nil, driver.ErrBadConn
	}

	// start transaction
	c.log("begin transaction tx=", c.inTransaction)

	if c.inTransaction {
		return nil, sql.ErrTxDone
	}

	// TODO(xq262144): make use of the ctx argument
	c.inTransaction = true
	c.queries = c.queries[:0]

	return c, nil
}

// PrepareContext implements the driver.ConnPrepareContext.ConnPrepareContext method.
func (c *conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if atomic.LoadInt32(&c.closed) != 0 {
		return nil, driver.ErrBadConn
	}

	// prepare the statement
	return newStmt(c, query), nil
}

// ExecContext implements the driver.ExecerContext.ExecContext method.
func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (result driver.Result, err error) {
	if atomic.LoadInt32(&c.closed) != 0 {
		err = driver.ErrBadConn
		return
	}

	// TODO(xq262144): make use of the ctx argument
	sq := convertQuery(query, args)
	if _, err = c.addQuery(wt.WriteQuery, sq); err != nil {
		return
	}

	result = driver.ResultNoRows

	return
}

// QueryContext implements the driver.QueryerContext.QueryContext method.
func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (rows driver.Rows, err error) {
	if atomic.LoadInt32(&c.closed) != 0 {
		err = driver.ErrBadConn
		return
	}

	// TODO(xq262144): make use of the ctx argument
	sq := convertQuery(query, args)
	return c.addQuery(wt.ReadQuery, sq)
}

// Commit implements the driver.Tx.Commit method.
func (c *conn) Commit() (err error) {
	if atomic.LoadInt32(&c.closed) != 0 {
		return driver.ErrBadConn
	}

	if !c.inTransaction {
		return sql.ErrTxDone
	}

	defer func() {
		c.queries = c.queries[:0]
		c.inTransaction = false
	}()

	if len(c.queries) > 0 {
		// send query
		if _, err = c.sendQuery(wt.WriteQuery, c.queries); err != nil {
			return
		}
	}

	return
}

// Rollback implements the driver.Tx.Rollback method.
func (c *conn) Rollback() error {
	if atomic.LoadInt32(&c.closed) != 0 {
		return driver.ErrBadConn
	}

	if !c.inTransaction {
		return sql.ErrTxDone
	}

	defer func() {
		c.queries = c.queries[:0]
		c.inTransaction = false
	}()

	if len(c.queries) == 0 {
		return sql.ErrTxDone
	}

	return nil
}

func (c *conn) addQuery(queryType wt.QueryType, query *storage.Query) (rows driver.Rows, err error) {
	if c.inTransaction {
		// check query type, enqueue query
		if queryType == wt.ReadQuery {
			// read query is not supported in transaction
			err = ErrQueryInTransaction
			return
		}

		// append queries
		c.queries = append(c.queries, *query)
		return
	}

	return c.sendQuery(queryType, []storage.Query{*query})
}

func (c *conn) sendQuery(queryType wt.QueryType, queries []storage.Query) (rows driver.Rows, err error) {
	c.peersLock.RLock()
	defer c.peersLock.RUnlock()

	// build request
	seqNo := atomic.AddUint64(&c.seqNo, 1)
	req := &wt.Request{
		Header: wt.SignedRequestHeader{
			RequestHeader: wt.RequestHeader{
				QueryType:    queryType,
				NodeID:       c.nodeID,
				DatabaseID:   c.dbID,
				ConnectionID: c.connectionID,
				SeqNo:        seqNo,
				Timestamp:    getLocalTime(),
			},
			Signee: c.pubKey,
		},
		Payload: wt.RequestPayload{
			Queries: queries,
		},
	}

	if err = req.Sign(c.privKey); err != nil {
		return
	}

	var response wt.Response
	if err = rpc.NewCaller().CallNode(c.peers.Leader.ID, "DBS.Query", req, &response); err != nil {
		return
	}

	// verify response
	if err = response.Verify(); err != nil {
		return
	}

	// build ack
	ack := &wt.Ack{
		Header: wt.SignedAckHeader{
			AckHeader: wt.AckHeader{
				Response:  response.Header,
				NodeID:    c.nodeID,
				Timestamp: getLocalTime(),
			},
			Signee: c.pubKey,
		},
	}

	if err = ack.Sign(c.privKey); err != nil {
		return
	}

	var ackRes wt.AckResponse

	// send ack back
	if err = rpc.NewCaller().CallNode(c.peers.Leader.ID, "DBS.Ack", ack, &ackRes); err != nil {
		return
	}

	rows = newRows(&response)

	return
}

func (c *conn) getPeers() (err error) {
	c.peersLock.Lock()
	defer c.peersLock.Unlock()

	req := &bp.GetDatabaseRequest{
		DatabaseID: c.dbID,
	}
	res := new(bp.GetDatabaseResponse)
	if err = requestBP(bp.DBServiceName+".GetDatabase", req, res); err != nil {
		return
	}

	c.peers = res.InstanceMeta.Peers

	return
}

func getLocalTime() time.Time {
	return time.Now().UTC()
}

func convertQuery(query string, args []driver.NamedValue) (sq *storage.Query) {
	// rebuild args to named args
	sq = &storage.Query{
		Pattern: query,
	}

	sq.Args = make([]sql.NamedArg, len(args))

	for i, v := range args {
		sq.Args[i] = sql.Named(v.Name, v.Value)
	}

	return
}