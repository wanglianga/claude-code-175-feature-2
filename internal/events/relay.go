// Package events 负责把事务性 outbox(lease_events) 投递到 Kafka。
// 投递成功才标记 published; 失败重试, 因此是 at-least-once, 消费方必须按 event_id 幂等。
package events

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
)

type Relay struct {
	pool   *pgxpool.Pool
	writer *kafka.Writer
	topic  string
}

func NewRelay(pool *pgxpool.Pool, writer *kafka.Writer, topic string) *Relay {
	return &Relay{pool: pool, writer: writer, topic: topic}
}

// Run 阻塞运行, 直到 ctx 取消。
func (r *Relay) Run(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.pump(ctx); err != nil {
				log.Printf("[relay] pump error: %v", err)
			}
		}
	}
}

func (r *Relay) pump(ctx context.Context) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT id, event_id::text, contract_id::text, event_type, payload, actor_id::text, created_at
		FROM lease_events
		WHERE published = false
		ORDER BY id
		LIMIT 100
		FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return err
	}
	type ev struct {
		id         int64
		eventID    string
		contractID string
		etype      string
		payload    []byte
		actorID    string
		createdAt  time.Time
	}
	var batch []ev
	for rows.Next() {
		var e ev
		var actor *string
		if err := rows.Scan(&e.id, &e.eventID, &e.contractID, &e.etype, &e.payload, &actor, &e.createdAt); err != nil {
			rows.Close()
			return err
		}
		if actor != nil {
			e.actorID = *actor
		}
		batch = append(batch, e)
	}
	rows.Close()
	if len(batch) == 0 {
		return nil
	}

	msgs := make([]kafka.Message, 0, len(batch))
	for _, e := range batch {
		envelope := map[string]any{
			"event_id":    e.eventID,
			"contract_id": e.contractID,
			"event_type":  e.etype,
			"payload":     json.RawMessage(e.payload),
			"actor_id":    e.actorID,
			"occurred_at": e.createdAt,
		}
		b, _ := json.Marshal(envelope)
		msgs = append(msgs, kafka.Message{
			Key:   []byte(e.contractID), // 同一租赁同一分区, 保证有序
			Value: b,
		})
	}
	if err := r.writer.WriteMessages(ctx, msgs...); err != nil {
		return err
	}
	for i, e := range batch {
		if _, err := tx.Exec(ctx, `
			UPDATE lease_events SET published=true, published_at=now(),
				kafka_partition=$2, kafka_offset=$3
			WHERE id=$1`, e.id, msgs[i].Partition, msgs[i].Offset); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	log.Printf("[relay] published %d events -> %s", len(batch), r.topic)
	return nil
}
