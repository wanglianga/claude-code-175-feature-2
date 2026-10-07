package app

import "context"

func (s *Service) PingDB(ctx context.Context) error {
	return s.pool.Ping(ctx)
}
