package app

import "time"

// reapLoop deletes expired dumpsters and closes their subscribers. store.Get
// already hides expired ids, so this loop only reclaims memory.
func (a *App) reapLoop() {
	ticker := time.NewTicker(a.cfg.ReapEvery)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case now := <-ticker.C:
			for _, sid := range a.store.DeleteExpired(now) {
				a.hub.Close(sid)
				a.log.Info("dumpster reaped", "id", sid)
			}
			a.rl.Prune()
		}
	}
}
