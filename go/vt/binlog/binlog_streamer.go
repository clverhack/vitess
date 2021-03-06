// Copyright 2014, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package binlog

import (
	"fmt"
	"io"
	"strings"

	log "github.com/golang/glog"
	"github.com/youtube/vitess/go/stats"
	"github.com/youtube/vitess/go/sync2"
	"github.com/youtube/vitess/go/vt/mysqlctl"
	"github.com/youtube/vitess/go/vt/mysqlctl/replication"

	binlogdatapb "github.com/youtube/vitess/go/vt/proto/binlogdata"
)

var (
	binlogStreamerErrors = stats.NewCounters("BinlogStreamerErrors")

	// ErrClientEOF is returned by Streamer if the stream ended because the
	// consumer of the stream indicated it doesn't want any more events.
	ErrClientEOF = fmt.Errorf("binlog stream consumer ended the reply stream")
	// ErrServerEOF is returned by Streamer if the stream ended because the
	// connection to the mysqld server was lost, or the stream was terminated by
	// mysqld.
	ErrServerEOF = fmt.Errorf("binlog stream connection was closed by mysqld")

	// statementPrefixes are normal sql statement prefixes.
	statementPrefixes = map[string]binlogdatapb.BinlogTransaction_Statement_Category{
		"begin":    binlogdatapb.BinlogTransaction_Statement_BL_BEGIN,
		"commit":   binlogdatapb.BinlogTransaction_Statement_BL_COMMIT,
		"rollback": binlogdatapb.BinlogTransaction_Statement_BL_ROLLBACK,
		"insert":   binlogdatapb.BinlogTransaction_Statement_BL_DML,
		"update":   binlogdatapb.BinlogTransaction_Statement_BL_DML,
		"delete":   binlogdatapb.BinlogTransaction_Statement_BL_DML,
		"create":   binlogdatapb.BinlogTransaction_Statement_BL_DDL,
		"alter":    binlogdatapb.BinlogTransaction_Statement_BL_DDL,
		"drop":     binlogdatapb.BinlogTransaction_Statement_BL_DDL,
		"truncate": binlogdatapb.BinlogTransaction_Statement_BL_DDL,
		"rename":   binlogdatapb.BinlogTransaction_Statement_BL_DDL,
		"set":      binlogdatapb.BinlogTransaction_Statement_BL_SET,
	}
)

// sendTransactionFunc is used to send binlog events.
// reply is of type binlogdatapb.BinlogTransaction.
type sendTransactionFunc func(trans *binlogdatapb.BinlogTransaction) error

// getStatementCategory returns the binlogdatapb.BL_* category for a SQL statement.
func getStatementCategory(sql string) binlogdatapb.BinlogTransaction_Statement_Category {
	if i := strings.IndexByte(sql, byte(' ')); i >= 0 {
		sql = sql[:i]
	}
	return statementPrefixes[strings.ToLower(sql)]
}

// Streamer streams binlog events from MySQL by connecting as a slave.
// A Streamer should only be used once. To start another stream, call
// NewStreamer() again.
type Streamer struct {
	// dbname and mysqld are set at creation.
	dbname          string
	mysqld          mysqlctl.MysqlDaemon
	clientCharset   *binlogdatapb.Charset
	startPos        replication.Position
	sendTransaction sendTransactionFunc

	conn *mysqlctl.SlaveConnection
}

// NewStreamer creates a binlog Streamer.
//
// dbname specifes the database to stream events for.
// mysqld is the local instance of mysqlctl.Mysqld.
// charset is the default character set on the BinlogPlayer side.
// startPos is the position to start streaming at.
// sendTransaction is called each time a transaction is committed or rolled back.
func NewStreamer(dbname string, mysqld mysqlctl.MysqlDaemon, clientCharset *binlogdatapb.Charset, startPos replication.Position, sendTransaction sendTransactionFunc) *Streamer {
	return &Streamer{
		dbname:          dbname,
		mysqld:          mysqld,
		clientCharset:   clientCharset,
		startPos:        startPos,
		sendTransaction: sendTransaction,
	}
}

// Stream starts streaming binlog events using the settings from NewStreamer().
func (bls *Streamer) Stream(ctx *sync2.ServiceContext) (err error) {
	stopPos := bls.startPos
	defer func() {
		if err != nil {
			err = fmt.Errorf("stream error @ %v: %v", stopPos, err)
		}
		log.Infof("stream ended @ %v, err = %v", stopPos, err)
	}()

	if bls.conn, err = bls.mysqld.NewSlaveConnection(); err != nil {
		return err
	}
	defer bls.conn.Close()

	// Check that the default charsets match, if the client specified one.
	// Note that Streamer uses the settings for the 'dba' user, while
	// BinlogPlayer uses the 'filtered' user, so those are the ones whose charset
	// must match. Filtered replication should still succeed even with a default
	// mismatch, since we pass per-statement charset info. However, Vitess in
	// general doesn't support servers with different default charsets, so we
	// treat it as a configuration error.
	if bls.clientCharset != nil {
		cs, err := bls.conn.GetCharset()
		if err != nil {
			return fmt.Errorf("can't get charset to check binlog stream: %v", err)
		}
		log.Infof("binlog stream client charset = %v, server charset = %v", *bls.clientCharset, cs)
		if *cs != *bls.clientCharset {
			return fmt.Errorf("binlog stream client charset (%v) doesn't match server (%v)", bls.clientCharset, cs)
		}
	}

	var events <-chan replication.BinlogEvent
	events, err = bls.conn.StartBinlogDump(bls.startPos)
	if err != nil {
		return err
	}
	// parseEvents will loop until the events channel is closed, the
	// service enters the SHUTTING_DOWN state, or an error occurs.
	stopPos, err = bls.parseEvents(ctx, events)
	return err
}

// parseEvents processes the raw binlog dump stream from the server, one event
// at a time, and groups them into transactions. It is called from within the
// service function launched by Stream().
//
// If the sendTransaction func returns io.EOF, parseEvents returns ErrClientEOF.
// If the events channel is closed, parseEvents returns ErrServerEOF.
func (bls *Streamer) parseEvents(ctx *sync2.ServiceContext, events <-chan replication.BinlogEvent) (replication.Position, error) {
	var statements []*binlogdatapb.BinlogTransaction_Statement
	var format replication.BinlogFormat
	var gtid replication.GTID
	var pos = bls.startPos
	var autocommit = true
	var err error

	// A begin can be triggered either by a BEGIN query, or by a GTID_EVENT.
	begin := func() {
		if statements != nil {
			// If this happened, it would be a legitimate error.
			log.Errorf("BEGIN in binlog stream while still in another transaction; dropping %d statements: %v", len(statements), statements)
			binlogStreamerErrors.Add("ParseEvents", 1)
		}
		statements = make([]*binlogdatapb.BinlogTransaction_Statement, 0, 10)
		autocommit = false
	}
	// A commit can be triggered either by a COMMIT query, or by an XID_EVENT.
	// Statements that aren't wrapped in BEGIN/COMMIT are committed immediately.
	commit := func(timestamp uint32) error {
		trans := &binlogdatapb.BinlogTransaction{
			Statements:    statements,
			Timestamp:     int64(timestamp),
			TransactionId: replication.EncodeGTID(gtid),
		}
		if err = bls.sendTransaction(trans); err != nil {
			if err == io.EOF {
				return ErrClientEOF
			}
			return fmt.Errorf("send reply error: %v", err)
		}
		statements = nil
		autocommit = true
		return nil
	}

	// Parse events.
	for ctx.IsRunning() {
		var ev replication.BinlogEvent
		var ok bool

		select {
		case ev, ok = <-events:
			if !ok {
				// events channel has been closed, which means the connection died.
				log.Infof("reached end of binlog event stream")
				return pos, ErrServerEOF
			}
		case <-ctx.ShuttingDown:
			log.Infof("stopping early due to binlog Streamer service shutdown")
			return pos, nil
		}

		// Validate the buffer before reading fields from it.
		if !ev.IsValid() {
			return pos, fmt.Errorf("can't parse binlog event, invalid data: %#v", ev)
		}

		// We need to keep checking for FORMAT_DESCRIPTION_EVENT even after we've
		// seen one, because another one might come along (e.g. on log rotate due to
		// binlog settings change) that changes the format.
		if ev.IsFormatDescription() {
			format, err = ev.Format()
			if err != nil {
				return pos, fmt.Errorf("can't parse FORMAT_DESCRIPTION_EVENT: %v, event data: %#v", err, ev)
			}
			continue
		}

		// We can't parse anything until we get a FORMAT_DESCRIPTION_EVENT that
		// tells us the size of the event header.
		if format.IsZero() {
			// The only thing that should come before the FORMAT_DESCRIPTION_EVENT
			// is a fake ROTATE_EVENT, which the master sends to tell us the name
			// of the current log file.
			if ev.IsRotate() {
				continue
			}
			return pos, fmt.Errorf("got a real event before FORMAT_DESCRIPTION_EVENT: %#v", ev)
		}

		// Strip the checksum, if any. We don't actually verify the checksum, so discard it.
		ev, _, err = ev.StripChecksum(format)
		if err != nil {
			return pos, fmt.Errorf("can't strip checksum from binlog event: %v, event data: %#v", err, ev)
		}

		// Update the GTID if the event has one. The actual event type could be
		// something special like GTID_EVENT (MariaDB, MySQL 5.6), or it could be
		// an arbitrary event with a GTID in the header (Google MySQL).
		if ev.HasGTID(format) {
			gtid, err = ev.GTID(format)
			if err != nil {
				return pos, fmt.Errorf("can't get GTID from binlog event: %v, event data: %#v", err, ev)
			}
			pos = replication.AppendGTID(pos, gtid)
		}

		switch {
		case ev.IsGTID(): // GTID_EVENT
			if ev.IsBeginGTID(format) {
				begin()
			}
		case ev.IsXID(): // XID_EVENT (equivalent to COMMIT)
			if err = commit(ev.Timestamp()); err != nil {
				return pos, err
			}
		case ev.IsIntVar(): // INTVAR_EVENT
			name, value, err := ev.IntVar(format)
			if err != nil {
				return pos, fmt.Errorf("can't parse INTVAR_EVENT: %v, event data: %#v", err, ev)
			}
			statements = append(statements, &binlogdatapb.BinlogTransaction_Statement{
				Category: binlogdatapb.BinlogTransaction_Statement_BL_SET,
				Sql:      fmt.Sprintf("SET %s=%d", name, value),
			})
		case ev.IsRand(): // RAND_EVENT
			seed1, seed2, err := ev.Rand(format)
			if err != nil {
				return pos, fmt.Errorf("can't parse RAND_EVENT: %v, event data: %#v", err, ev)
			}
			statements = append(statements, &binlogdatapb.BinlogTransaction_Statement{
				Category: binlogdatapb.BinlogTransaction_Statement_BL_SET,
				Sql:      fmt.Sprintf("SET @@RAND_SEED1=%d, @@RAND_SEED2=%d", seed1, seed2),
			})
		case ev.IsQuery(): // QUERY_EVENT
			// Extract the query string and group into transactions.
			q, err := ev.Query(format)
			if err != nil {
				return pos, fmt.Errorf("can't get query from binlog event: %v, event data: %#v", err, ev)
			}
			switch cat := getStatementCategory(q.SQL); cat {
			case binlogdatapb.BinlogTransaction_Statement_BL_BEGIN:
				begin()
			case binlogdatapb.BinlogTransaction_Statement_BL_ROLLBACK:
				// Rollbacks are possible under some circumstances. Since the stream
				// client keeps track of its replication position by updating the set
				// of GTIDs it's seen, we must commit an empty transaction so the client
				// can update its position.
				statements = nil
				fallthrough
			case binlogdatapb.BinlogTransaction_Statement_BL_COMMIT:
				if err = commit(ev.Timestamp()); err != nil {
					return pos, err
				}
			default: // BL_DDL, BL_DML, BL_SET, BL_UNRECOGNIZED
				if q.Database != "" && q.Database != bls.dbname {
					// Skip cross-db statements.
					continue
				}
				setTimestamp := &binlogdatapb.BinlogTransaction_Statement{
					Category: binlogdatapb.BinlogTransaction_Statement_BL_SET,
					Sql:      fmt.Sprintf("SET TIMESTAMP=%d", ev.Timestamp()),
				}
				statement := &binlogdatapb.BinlogTransaction_Statement{
					Category: cat,
					Sql:      q.SQL,
				}
				// If the statement has a charset and it's different than our client's
				// default charset, send it along with the statement.
				// If our client hasn't told us its charset, always send it.
				if bls.clientCharset == nil || (q.Charset != nil && *q.Charset != *bls.clientCharset) {
					setTimestamp.Charset = q.Charset
					statement.Charset = q.Charset
				}
				statements = append(statements, setTimestamp, statement)
				if autocommit {
					if err = commit(ev.Timestamp()); err != nil {
						return pos, err
					}
				}
			}
		}
	}

	return pos, nil
}
