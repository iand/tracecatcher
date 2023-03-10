package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/libp2p/go-libp2p/core/peer"
	"golang.org/x/exp/slog"
)

func connect(ctx context.Context,
	dbHost string,
	dbPort int,
	dbName string,
	dbSSLMode string,
	dbUser string,
	dbPassword string,
) (*pgx.Conn, error) {
	slog.Info("connecting to database", "host", dbHost, "port", dbPort, "dbname", dbName)

	dsn := fmt.Sprintf("host=%s port=%d dbname=%s sslmode=%s user=%s password=%s",
		dbHost, dbPort, dbName, dbSSLMode, dbUser, dbPassword)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgconn connect: %w", err)
	}

	if err := ensureDatabaseSchema(ctx, conn); err != nil {
		return nil, fmt.Errorf("ensure schema exists: %w", err)
	}

	return conn, nil
}

func ensureDatabaseSchema(ctx context.Context, conn *pgx.Conn) error {
	slog.Info("ensuring database schema exists")

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	for et, tbl := range eventDefs {
		if tbl.DDL == "" {
			slog.Debug("skipping event type, no ddl", "event_type", et.Key())
			continue
		}
		if tbl.BatchInsert == nil {
			slog.Debug("skipping event type, no batch insert function defined", "event_type", et.Key())
			continue
		}
		slog.Debug("ensuring event type tables exists", "event_type", et.Key())
		_, err = tx.Exec(ctx, tbl.DDL)
		if err != nil {
			return fmt.Errorf("exec ddl for %s: %w", et.Key(), err)
		}
	}

	err = tx.Commit(context.Background())
	if err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

type BatchInsertFunc func(context.Context, []*TraceEvent) (*pgx.Batch, error)

type EventDef struct {
	Name        string
	DDL         string
	BatchInsert BatchInsertFunc
}

var eventDefs = map[EventType]EventDef{
	EventTypePublishMessage: {
		Name: "publish_message_event",
		DDL: `
			CREATE TABLE IF NOT EXISTS publish_message_event (
			    id               INT         GENERATED ALWAYS AS IDENTITY,
				peer_id          TEXT        NOT NULL,
				timestamp        TIMESTAMPTZ NOT NULL,
				message_id       TEXT        NOT NULL,
				topic            TEXT        NOT NULL,
			    PRIMARY KEY (id)
			);

			CREATE INDEX IF NOT EXISTS idx_publish_message_event_timestamp ON publish_message_event (timestamp);
			CREATE INDEX IF NOT EXISTS idx_publish_message_event_peer_id   ON publish_message_event (peer_id);
			CREATE INDEX IF NOT EXISTS idx_publish_message_event_topic     ON publish_message_event USING hash (topic);
		`,
		BatchInsert: func(ctx context.Context, evs []*TraceEvent) (*pgx.Batch, error) {
			logger := slog.With("event_type", "publish_message")
			b := new(pgx.Batch)

			cols := []string{"peer_id", "timestamp", "message_id", "topic"}

			values := make([]any, 0, len(evs)*len(cols))
			rowCount := 0
			for _, ev := range evs {
				if ev.Timestamp == nil {
					logger.Debug("skipping event, no timestamp")
					continue
				}
				sub := ev.PublishMessage
				if sub == nil {
					logger.Debug("skipping event, not a publish message event", "type", ev.Type)
					continue
				}

				peerID, err := peer.IDFromBytes([]byte(ev.PeerID))
				if err != nil {
					logger.Debug("skipping event, bad peer id", "peer_id", ev.PeerID)
					continue
				}

				rowCount++
				values = append(values, peerID.String())
				values = append(values, time.Unix(0, *ev.Timestamp))
				values = append(values, string(sub.MessageID))
				values = append(values, derefString(sub.Topic, ""))
			}

			sql := buildBulkInsert("publish_message_event", cols, rowCount)
			b.Queue(sql, values...)
			return b, nil
		},
	},

	EventTypeRejectMessage: {
		Name: "reject_message_event",
		DDL: `
			CREATE TABLE IF NOT EXISTS reject_message_event (
			    id               INT         GENERATED ALWAYS AS IDENTITY,
				peer_id          TEXT        NOT NULL,
				timestamp        TIMESTAMPTZ NOT NULL,
				message_id       TEXT        NOT NULL,
				topic            TEXT        NOT NULL,
				received_from    TEXT        NOT NULL,
				reason           TEXT        NOT NULL,
			    PRIMARY KEY (id)
			);

			CREATE INDEX IF NOT EXISTS idx_reject_message_event_timestamp       ON reject_message_event (timestamp);
			CREATE INDEX IF NOT EXISTS idx_reject_message_event_peer_id         ON reject_message_event (peer_id);
			CREATE INDEX IF NOT EXISTS idx_reject_message_event_topic           ON reject_message_event USING hash (topic);
			CREATE INDEX IF NOT EXISTS idx_reject_message_event_received_from   ON reject_message_event (received_from);
		`,
		BatchInsert: func(ctx context.Context, evs []*TraceEvent) (*pgx.Batch, error) {
			logger := slog.With("event_type", "reject_message")
			b := new(pgx.Batch)

			cols := []string{"peer_id", "timestamp", "message_id", "topic", "received_from", "reason"}

			values := make([]any, 0, len(evs)*len(cols))
			rowCount := 0
			for _, ev := range evs {
				if ev.Timestamp == nil {
					logger.Debug("skipping event, no timestamp")
					continue
				}
				sub := ev.RejectMessage
				if sub == nil {
					logger.Debug("skipping event, not a reject message event", "type", ev.Type)
					continue
				}

				peerID, err := peer.IDFromBytes([]byte(ev.PeerID))
				if err != nil {
					logger.Debug("skipping event, bad peer id", "peer_id", ev.PeerID)
					continue
				}

				receivedFromPeerID, err := peer.IDFromBytes([]byte(sub.ReceivedFrom))
				if err != nil {
					logger.Debug("skipping event, bad received from peer id", "peer_id", sub.ReceivedFrom)
					continue
				}

				rowCount++
				values = append(values, peerID.String())
				values = append(values, time.Unix(0, *ev.Timestamp))
				values = append(values, string(sub.MessageID))
				values = append(values, derefString(sub.Topic, ""))
				values = append(values, receivedFromPeerID.String())
				values = append(values, derefString(sub.Reason, ""))
			}

			sql := buildBulkInsert("reject_message_event", cols, rowCount)
			b.Queue(sql, values...)
			return b, nil
		},
	},

	EventTypeDuplicateMessage: {
		Name: "duplicate_message_event",
		DDL: `
			CREATE TABLE IF NOT EXISTS duplicate_message_event (
			    id               INT         GENERATED ALWAYS AS IDENTITY,
				peer_id          TEXT        NOT NULL,
				timestamp        TIMESTAMPTZ NOT NULL,
				message_id       TEXT        NOT NULL,
				topic            TEXT        NOT NULL,
				received_from    TEXT        NOT NULL,
			    PRIMARY KEY (id)
			);

			CREATE INDEX IF NOT EXISTS idx_duplicate_message_event_timestamp       ON duplicate_message_event (timestamp);
			CREATE INDEX IF NOT EXISTS idx_duplicate_message_event_peer_id         ON duplicate_message_event (peer_id);
			CREATE INDEX IF NOT EXISTS idx_duplicate_message_event_topic           ON duplicate_message_event USING hash (topic);
			CREATE INDEX IF NOT EXISTS idx_duplicate_message_event_received_from   ON duplicate_message_event (received_from);
		`,
		BatchInsert: func(ctx context.Context, evs []*TraceEvent) (*pgx.Batch, error) {
			logger := slog.With("event_type", "duplicate_message")
			b := new(pgx.Batch)

			cols := []string{"peer_id", "timestamp", "message_id", "topic", "received_from"}

			values := make([]any, 0, len(evs)*len(cols))
			rowCount := 0
			for _, ev := range evs {
				if ev.Timestamp == nil {
					logger.Debug("skipping event, no timestamp")
					continue
				}
				sub := ev.DuplicateMessage
				if sub == nil {
					logger.Debug("skipping event, not a duplicate message event", "type", ev.Type)
					continue
				}

				peerID, err := peer.IDFromBytes([]byte(ev.PeerID))
				if err != nil {
					logger.Debug("skipping event, bad peer id", "peer_id", ev.PeerID)
					continue
				}

				receivedFromPeerID, err := peer.IDFromBytes([]byte(sub.ReceivedFrom))
				if err != nil {
					logger.Debug("skipping event, bad received from peer id", "peer_id", sub.ReceivedFrom)
					continue
				}

				rowCount++
				values = append(values, peerID.String())
				values = append(values, time.Unix(0, *ev.Timestamp))
				values = append(values, string(sub.MessageID))
				values = append(values, derefString(sub.Topic, ""))
				values = append(values, receivedFromPeerID.String())
			}

			sql := buildBulkInsert("duplicate_message_event", cols, rowCount)
			b.Queue(sql, values...)
			return b, nil
		},
	},

	EventTypeDeliverMessage: {
		Name: "deliver_message_event",
		DDL: `
			CREATE TABLE IF NOT EXISTS deliver_message_event (
			    id               INT         GENERATED ALWAYS AS IDENTITY,
				peer_id          TEXT        NOT NULL,
				timestamp        TIMESTAMPTZ NOT NULL,
				message_id       TEXT        NOT NULL,
				topic            TEXT        NOT NULL,
				received_from    TEXT        NOT NULL,
			    PRIMARY KEY (id)
			);

			CREATE INDEX IF NOT EXISTS idx_deliver_message_event_timestamp       ON deliver_message_event (timestamp);
			CREATE INDEX IF NOT EXISTS idx_deliver_message_event_peer_id         ON deliver_message_event (peer_id);
			CREATE INDEX IF NOT EXISTS idx_deliver_message_event_topic           ON deliver_message_event USING hash (topic);
			CREATE INDEX IF NOT EXISTS idx_deliver_message_event_received_from   ON deliver_message_event (received_from);
		`,
		BatchInsert: func(ctx context.Context, evs []*TraceEvent) (*pgx.Batch, error) {
			logger := slog.With("event_type", "deliver_message")
			b := new(pgx.Batch)

			cols := []string{"peer_id", "timestamp", "message_id", "topic", "received_from"}

			values := make([]any, 0, len(evs)*len(cols))
			rowCount := 0
			for _, ev := range evs {
				if ev.Timestamp == nil {
					logger.Debug("skipping event, no timestamp")
					continue
				}
				sub := ev.DeliverMessage
				if sub == nil {
					logger.Debug("skipping event, not a deliver message event", "type", ev.Type)
					continue
				}

				peerID, err := peer.IDFromBytes([]byte(ev.PeerID))
				if err != nil {
					logger.Debug("skipping event, bad peer id", "peer_id", ev.PeerID)
					continue
				}

				receivedFromPeerID, err := peer.IDFromBytes([]byte(sub.ReceivedFrom))
				if err != nil {
					logger.Debug("skipping event, bad received from peer id", "peer_id", sub.ReceivedFrom)
					continue
				}

				rowCount++
				values = append(values, peerID.String())
				values = append(values, time.Unix(0, *ev.Timestamp))
				values = append(values, string(sub.MessageID))
				values = append(values, derefString(sub.Topic, ""))
				values = append(values, receivedFromPeerID.String())
			}

			sql := buildBulkInsert("deliver_message_event", cols, rowCount)
			b.Queue(sql, values...)
			return b, nil
		},
	},

	EventTypeAddPeer: {
		Name: "add_peer_event",
		DDL: `
			CREATE TABLE IF NOT EXISTS add_peer_event (
			    id               INT         GENERATED ALWAYS AS IDENTITY,
				peer_id          TEXT        NOT NULL,
				timestamp        TIMESTAMPTZ NOT NULL,
				other_peer_id    TEXT        NOT NULL,
				proto            TEXT        NOT NULL,
			    PRIMARY KEY (id)
			);

			CREATE INDEX IF NOT EXISTS idx_add_peer_event_timestamp       ON add_peer_event (timestamp);
			CREATE INDEX IF NOT EXISTS idx_add_peer_event_peer_id         ON add_peer_event (peer_id);
			CREATE INDEX IF NOT EXISTS idx_add_peer_event_other_peer_id   ON add_peer_event (other_peer_id);
		`,
		BatchInsert: func(ctx context.Context, evs []*TraceEvent) (*pgx.Batch, error) {
			logger := slog.With("event_type", "add_peer")
			b := new(pgx.Batch)

			cols := []string{"peer_id", "timestamp", "other_peer_id", "proto"}

			values := make([]any, 0, len(evs)*len(cols))
			rowCount := 0
			for _, ev := range evs {
				if ev.Timestamp == nil {
					logger.Debug("skipping event, no timestamp")
					continue
				}
				sub := ev.AddPeer
				if sub == nil {
					logger.Debug("skipping event, not an add peer event", "type", ev.Type)
					continue
				}

				peerID, err := peer.IDFromBytes(ev.PeerID)
				if err != nil {
					logger.Debug("skipping event, bad peer id", "peer_id", ev.PeerID)
					continue
				}

				otherPeerID, err := peer.IDFromBytes(sub.PeerID)
				if err != nil {
					logger.Debug("skipping event, bad received from peer id", "peer_id", sub.PeerID)
					continue
				}

				rowCount++
				values = append(values, peerID.String())
				values = append(values, time.Unix(0, *ev.Timestamp))
				values = append(values, otherPeerID.String())
				values = append(values, derefString(ev.AddPeer.Proto, ""))
			}

			sql := buildBulkInsert("add_peer_event", cols, rowCount)
			b.Queue(sql, values...)
			return b, nil
		},
	},

	EventTypeRemovePeer: {
		Name: "remove_peer_event",
		DDL: `
			CREATE TABLE IF NOT EXISTS remove_peer_event (
			    id               INT         GENERATED ALWAYS AS IDENTITY,
				peer_id          TEXT        NOT NULL,
				timestamp        TIMESTAMPTZ NOT NULL,
				other_peer_id    TEXT        NOT NULL,
			    PRIMARY KEY (id)
			);

			CREATE INDEX IF NOT EXISTS idx_remove_peer_event_timestamp       ON remove_peer_event (timestamp);
			CREATE INDEX IF NOT EXISTS idx_remove_peer_event_peer_id         ON remove_peer_event (peer_id);
			CREATE INDEX IF NOT EXISTS idx_remove_peer_event_other_peer_id   ON remove_peer_event (other_peer_id);
		`,
		BatchInsert: func(ctx context.Context, evs []*TraceEvent) (*pgx.Batch, error) {
			logger := slog.With("event_type", "remove_peer")
			b := new(pgx.Batch)

			cols := []string{"peer_id", "timestamp", "other_peer_id"}

			values := make([]any, 0, len(evs)*len(cols))
			rowCount := 0
			for _, ev := range evs {
				if ev.Timestamp == nil {
					logger.Debug("skipping event, no timestamp")
					continue
				}
				sub := ev.RemovePeer
				if sub == nil {
					logger.Debug("skipping event, not a remove peer event", "type", ev.Type)
					continue
				}

				peerID, err := peer.IDFromBytes(ev.PeerID)
				if err != nil {
					logger.Debug("skipping event, bad peer id", "peer_id", ev.PeerID)
					continue
				}

				otherPeerID, err := peer.IDFromBytes(sub.PeerID)
				if err != nil {
					logger.Debug("skipping event, bad received from peer id", "peer_id", sub.PeerID)
					continue
				}

				rowCount++
				values = append(values, peerID.String())
				values = append(values, time.Unix(0, *ev.Timestamp))
				values = append(values, otherPeerID.String())
			}

			sql := buildBulkInsert("remove_peer_event", cols, rowCount)
			b.Queue(sql, values...)
			return b, nil
		},
	},

	EventTypeJoin: {
		Name: "join_event",
		DDL: `
			CREATE TABLE IF NOT EXISTS join_event (
			    id               INT         GENERATED ALWAYS AS IDENTITY,
				peer_id          TEXT        NOT NULL,
				timestamp        TIMESTAMPTZ NOT NULL,
				topic            TEXT        NOT NULL,
			    PRIMARY KEY (id)
			);

			CREATE INDEX IF NOT EXISTS idx_join_event_timestamp  ON join_event (timestamp);
			CREATE INDEX IF NOT EXISTS idx_join_event_peer_id    ON join_event (peer_id);
			CREATE INDEX IF NOT EXISTS idx_join_event_topic      ON join_event USING hash (topic);
		`,

		BatchInsert: func(ctx context.Context, evs []*TraceEvent) (*pgx.Batch, error) {
			logger := slog.With("event_type", "join")
			b := new(pgx.Batch)

			cols := []string{"peer_id", "timestamp", "topic"}

			values := make([]any, 0, len(evs)*len(cols))
			rowCount := 0
			for _, ev := range evs {
				if ev.Timestamp == nil {
					logger.Debug("skipping event, no timestamp")
					continue
				}
				sub := ev.Join
				if sub == nil {
					logger.Debug("skipping event, not a join event", "type", ev.Type)
					continue
				}

				peerID, err := peer.IDFromBytes([]byte(ev.PeerID))
				if err != nil {
					logger.Debug("skipping event, bad peer id", "peer_id", ev.PeerID)
					continue
				}

				rowCount++
				values = append(values, peerID.String())
				values = append(values, time.Unix(0, *ev.Timestamp))
				values = append(values, derefString(sub.Topic, ""))
			}

			sql := buildBulkInsert("join_event", cols, rowCount)
			b.Queue(sql, values...)
			return b, nil
		},
	},

	EventTypeLeave: {
		Name: "leave_event",
		DDL: `
			CREATE TABLE IF NOT EXISTS leave_event (
			    id               INT         GENERATED ALWAYS AS IDENTITY,
				peer_id          TEXT        NOT NULL,
				timestamp        TIMESTAMPTZ NOT NULL,
				topic            TEXT        NOT NULL,
			    PRIMARY KEY (id)
			);

			CREATE INDEX IF NOT EXISTS idx_leave_event_timestamp  ON leave_event (timestamp);
			CREATE INDEX IF NOT EXISTS idx_leave_event_peer_id    ON leave_event (peer_id);
			CREATE INDEX IF NOT EXISTS idx_leave_event_topic      ON leave_event USING hash (topic);
		`,
		BatchInsert: func(ctx context.Context, evs []*TraceEvent) (*pgx.Batch, error) {
			logger := slog.With("event_type", "leave")
			b := new(pgx.Batch)

			cols := []string{"peer_id", "timestamp", "topic"}

			values := make([]any, 0, len(evs)*len(cols))
			rowCount := 0
			for _, ev := range evs {
				if ev.Timestamp == nil {
					logger.Debug("skipping event, no timestamp")
					continue
				}
				sub := ev.Leave
				if sub == nil {
					logger.Debug("skipping event, not a leave event", "type", ev.Type)
					continue
				}

				peerID, err := peer.IDFromBytes([]byte(ev.PeerID))
				if err != nil {
					logger.Debug("skipping event, bad peer id", "peer_id", ev.PeerID)
					continue
				}

				rowCount++
				values = append(values, peerID.String())
				values = append(values, time.Unix(0, *ev.Timestamp))
				values = append(values, derefString(sub.Topic, ""))
			}

			sql := buildBulkInsert("leave_event", cols, rowCount)
			b.Queue(sql, values...)
			return b, nil
		},
	},

	EventTypeGraft: {
		Name: "graft_event",
		DDL: `
			CREATE TABLE IF NOT EXISTS graft_event (
			    id               INT         GENERATED ALWAYS AS IDENTITY,
				peer_id          TEXT        NOT NULL,
				timestamp        TIMESTAMPTZ NOT NULL,
				topic            TEXT        NOT NULL,
				other_peer_id    TEXT        NOT NULL,
			    PRIMARY KEY (id)
			);

			CREATE INDEX IF NOT EXISTS idx_graft_event_timestamp       ON graft_event (timestamp);
			CREATE INDEX IF NOT EXISTS idx_graft_event_peer_id         ON graft_event (peer_id);
			CREATE INDEX IF NOT EXISTS idx_graft_event_topic           ON graft_event USING hash (topic);
			CREATE INDEX IF NOT EXISTS idx_graft_event_other_peer_id   ON graft_event (other_peer_id);
		`,

		BatchInsert: func(ctx context.Context, evs []*TraceEvent) (*pgx.Batch, error) {
			logger := slog.With("event_type", "graft")
			b := new(pgx.Batch)

			cols := []string{"peer_id", "timestamp", "topic", "other_peer_id"}

			values := make([]any, 0, len(evs)*len(cols))
			rowCount := 0
			for _, ev := range evs {
				if ev.Timestamp == nil {
					logger.Debug("skipping event, no timestamp")
					continue
				}
				sub := ev.Graft
				if sub == nil {
					logger.Debug("skipping event, not a graft event", "type", ev.Type)
					continue
				}

				peerID, err := peer.IDFromBytes([]byte(ev.PeerID))
				if err != nil {
					logger.Debug("skipping event, bad peer id", "peer_id", ev.PeerID)
					continue
				}

				otherPeerID, err := peer.IDFromBytes(sub.PeerID)
				if err != nil {
					logger.Debug("skipping event, bad other peer id", "other_peer_id", sub.PeerID)
					continue
				}

				rowCount++
				values = append(values,
					peerID.String(),
					time.Unix(0, *ev.Timestamp),
					derefString(sub.Topic, ""),
					otherPeerID.String(),
				)
			}

			sql := buildBulkInsert("graft_event", cols, rowCount)
			b.Queue(sql, values...)
			return b, nil
		},
	},

	EventTypePrune: {
		Name: "prune_event",
		DDL: `
			CREATE TABLE IF NOT EXISTS prune_event (
			    id               INT         GENERATED ALWAYS AS IDENTITY,
				peer_id          TEXT        NOT NULL,
				timestamp        TIMESTAMPTZ NOT NULL,
				topic            TEXT        NOT NULL,
				other_peer_id    TEXT        NOT NULL,
			    PRIMARY KEY (id)
			);

			CREATE INDEX IF NOT EXISTS idx_prune_event_timestamp       ON prune_event (timestamp);
			CREATE INDEX IF NOT EXISTS idx_prune_event_peer_id         ON prune_event (peer_id);
			CREATE INDEX IF NOT EXISTS idx_prune_event_topic           ON prune_event USING hash (topic);
			CREATE INDEX IF NOT EXISTS idx_prune_event_other_peer_id   ON prune_event (other_peer_id);
		`,

		BatchInsert: func(ctx context.Context, evs []*TraceEvent) (*pgx.Batch, error) {
			logger := slog.With("event_type", "prune")
			b := new(pgx.Batch)

			cols := []string{"peer_id", "timestamp", "topic", "other_peer_id"}

			values := make([]any, 0, len(evs)*len(cols))
			rowCount := 0
			for _, ev := range evs {
				if ev.Timestamp == nil {
					logger.Debug("skipping event, no timestamp")
					continue
				}
				sub := ev.Prune
				if sub == nil {
					logger.Debug("skipping event, not a prune event", "type", ev.Type)
					continue
				}

				peerID, err := peer.IDFromBytes(ev.PeerID)
				if err != nil {
					logger.Debug("skipping event, bad peer id", "peer_id", ev.PeerID)
					continue
				}

				otherPeerID, err := peer.IDFromBytes(sub.PeerID)
				if err != nil {
					logger.Debug("skipping event, bad other peer id", "other_peer_id", sub.PeerID)
					continue
				}

				rowCount++
				values = append(values,
					peerID.String(),
					time.Unix(0, *ev.Timestamp),
					derefString(sub.Topic, ""),
					otherPeerID.String(),
				)
			}

			sql := buildBulkInsert("prune_event", cols, rowCount)
			b.Queue(sql, values...)
			return b, nil
		},
	},

	EventTypePeerScore: {
		Name: "peer_score_event",
		DDL: `
			CREATE TABLE IF NOT EXISTS peer_score_event (
			    id                    INT         GENERATED ALWAYS AS IDENTITY,
				peer_id               TEXT        NOT NULL,
				timestamp             TIMESTAMPTZ NOT NULL,
				other_peer_id         TEXT        NOT NULL,
				app_specific_score    FLOAT8      NOT NULL,
				ip_colocation_factor  FLOAT8      NOT NULL,
				behaviour_penalty     FLOAT8      NOT NULL,
			    PRIMARY KEY (id)
			);

			CREATE INDEX IF NOT EXISTS idx_peer_score_event_timestamp       ON peer_score_event (timestamp);
			CREATE INDEX IF NOT EXISTS idx_peer_score_event_peer_id         ON peer_score_event (peer_id);
			CREATE INDEX IF NOT EXISTS idx_peer_score_event_other_peer_id   ON peer_score_event (other_peer_id);

			CREATE TABLE IF NOT EXISTS peer_score_topic (
			    id                          INT         GENERATED ALWAYS AS IDENTITY,
			    peer_score_event_id         INT         NOT NULL,
				topic                       TEXT        NOT NULL,
				time_in_mesh                INTERVAL    NOT NULL,
				first_message_deliveries    FLOAT8      NOT NULL,
				mesh_message_deliveries     FLOAT8      NOT NULL,
				invalid_message_deliveries  FLOAT8      NOT NULL,
			    PRIMARY KEY (id)
			);

			CREATE INDEX IF NOT EXISTS idx_peer_score_topic_peer_score_event_id   ON peer_score_topic (peer_score_event_id);
			CREATE INDEX IF NOT EXISTS idx_peer_score_topic_topic                 ON peer_score_topic USING hash (topic);
		`,
		BatchInsert: func(ctx context.Context, evs []*TraceEvent) (*pgx.Batch, error) {
			logger := slog.With("event_type", "peer_score")
			b := new(pgx.Batch)

			parentCols := []string{"peer_id", "timestamp", "other_peer_id", "app_specific_score", "ip_colocation_factor", "behaviour_penalty"}
			childCols := []string{"peer_score_event_id", "topic", "time_in_mesh", "first_message_deliveries", "mesh_message_deliveries", "invalid_message_deliveries"}

			eventCount := 0
			for _, ev := range evs {
				if ev.Timestamp == nil {
					logger.Debug("skipping event, no timestamp")
					continue
				}
				sub := ev.PeerScore
				if sub == nil {
					logger.Debug("skipping event, not a peer score event", "type", ev.Type)
					continue
				}

				// TODO: remove this terrible hack caused by Lotus putting pretty peer ids into a byte slice and encoding to json
				// See https://github.com/filecoin-project/lotus/pull/10271
				peerID, err := peer.IDFromBytes(ev.PeerID)
				if err != nil {
					decoded, err := base64.StdEncoding.DecodeString(string(ev.PeerID))
					if err != nil {
						logger.Debug("skipping event, guessing peer id encoding", "error", err, "peer_id", ev.PeerID)
						continue
					}

					peerID, err = peer.IDFromBytes(decoded)
					if err != nil {
						logger.Debug("skipping event, bad peer id", "error", err, "peer_id", ev.PeerID)
						continue
					}
				}

				otherPeerID, err := peer.IDFromBytes(sub.PeerID)
				if err != nil {
					decoded, err := base64.StdEncoding.DecodeString(string(sub.PeerID))
					if err != nil {
						logger.Debug("skipping event, guessing peer id encoding", "error", err, "peer_id", ev.PeerID)
						continue
					}

					peerID, err = peer.IDFromBytes(decoded)
					if err != nil {
						logger.Debug("skipping event, bad peer id", "error", err, "peer_id", ev.PeerID)
						continue
					}
				}

				values := make([]any, 0, len(parentCols)+len(sub.Topics)*len(childCols))

				// add parent values
				values = append(values,
					peerID.String(),
					time.Unix(0, *ev.Timestamp),
					otherPeerID.String(),
					sub.AppSpecificScore,
					sub.IPColocationFactor,
					sub.BehaviourPenalty,
				)

				// add child values
				childRowCount := 0
				for _, t := range sub.Topics {
					childRowCount++
					values = append(values,
						t.Topic,
						t.TimeInMesh,
						t.FirstMessageDeliveries,
						t.MeshMessageDeliveries,
						t.InvalidMessageDeliveries,
					)
				}

				sql := buildBulkInsertParentChild("peer_score_event", parentCols, "peer_score_topic", childCols, childRowCount)
				b.Queue(sql, values...)
				eventCount++
			}
			return b, nil
		},
	},
}

func derefString(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}

func buildBulkInsert(table string, columns []string, rowCount int) string {
	var b strings.Builder
	b.WriteString("INSERT INTO " + table + "(" + strings.Join(columns, ", ") + ") VALUES ")
	idx := 0
	for r := 0; r < rowCount; r++ {
		if r > 0 {
			b.WriteString(",")
		}
		b.WriteString("(")
		for c := 0; c < len(columns); c++ {
			if c > 0 {
				b.WriteString(", ")
			}
			idx++
			b.WriteString("$")
			b.WriteString(strconv.Itoa(idx))
		}
		b.WriteString(")")
	}
	return b.String()
}

func buildBulkInsertParentChild(parentTable string, parentColumns []string, childTable string, childColumns []string, childRowCount int) string {
	// WITH new_peer_score_event AS (
	//     INSERT INTO peer_score_event(
	//     	peer_id,
	//     	timestamp,
	//     	other_peer_id,
	//     	app_specific_score,
	//     	ip_colocation_factor,
	//     	behaviour_penalty
	//     ) VALUES ($1, $2, $3, $4, $5, $6)
	//     RETURNING id
	// )

	// INSERT INTO peer_score_topic (peer_score_event_id, topic, time_in_mesh, first_message_deliveries, mesh_message_deliveries, invalid_message_deliveries)
	// VALUES (
	//	(select id from new_peer_score_event), $7, $8, $9, $10, $11),
	//	(select id from new_peer_score_event), $12, $13, $14, $15, $16)
	// )

	if childRowCount == 0 {
		return buildBulkInsert(parentTable, parentColumns, 1)
	}

	var b strings.Builder
	b.WriteString("WITH new_")
	b.WriteString(parentTable)
	b.WriteString(" AS (")
	b.WriteString("INSERT INTO " + parentTable + "(" + strings.Join(parentColumns, ", ") + ") VALUES ")

	idx := 0

	// Write placeholders for parent values
	b.WriteString("(")
	for c := 0; c < len(parentColumns); c++ {
		if c > 0 {
			b.WriteString(", ")
		}
		idx++
		b.WriteString("$")
		b.WriteString(strconv.Itoa(idx))
	}
	b.WriteString(") RETURNING id) ")
	b.WriteString("INSERT INTO " + childTable + "(" + strings.Join(childColumns, ", ") + ") VALUES ")

	// Write placeholders for child values
	for r := 0; r < childRowCount; r++ {
		if r > 0 {
			b.WriteString(",")
		}
		b.WriteString("(")
		b.WriteString("(select id from new_")
		b.WriteString(parentTable)
		b.WriteString(")")
		for c := 1; c < len(childColumns); c++ {
			idx++
			b.WriteString(",")
			b.WriteString("$")
			b.WriteString(strconv.Itoa(idx))
		}
		b.WriteString(")")
	}
	return b.String()
}
