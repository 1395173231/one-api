package election

import (
	"context"
	"fmt"
	"os"
	"time"

	"one-api/common/config"
	"one-api/common/logger"
	rds "one-api/common/redis"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/viper"
)

const leaderKey = "onehub:leader"

// Lua script: renew TTL only if we still own the lock (value matches)
const renewLua = `
local v = redis.call('GET', KEYS[1])
if v == ARGV[1] then
  return redis.call('PEXPIRE', KEYS[1], ARGV[2])
else
  return 0
end
`

// StartLeaderElection starts a background goroutine that:
// - competes for a Redis SETNX lease to become leader (master)
// - renews the lease while holding it
// - demotes to follower (slave) when lease cannot be renewed
//
// Behavior:
// - If Redis is not enabled, this function returns immediately and
//   the legacy node_type config continues to control IsMasterNode.
// - If Redis is enabled, automatic leader election runs and overrides
//   config.IsMasterNode according to the lease ownership.
func StartLeaderElection() {
	if !config.RedisEnabled {
		// Redis disabled: stick to configured node_type behavior
		return
	}
	if viper.IsSet("leader_election.enable") && !viper.GetBool("leader_election.enable") {
		logger.SysLog("Leader election disabled by config: leader_election.enable=false")
		return
	}

	client := rds.GetRedisClient()
	if client == nil {
		// Defensive: Redis said enabled but client not ready
		logger.SysError("Leader election skipped: Redis client not initialized")
		return
	}

	leaseSeconds := viper.GetInt("leader_election.lease_seconds")
	if leaseSeconds <= 0 {
		leaseSeconds = 15
	}
	leaseTTL := time.Duration(leaseSeconds) * time.Second
	// Renew at half the TTL (but no less than 1s)
	renewInterval := leaseTTL / 2
	if renewInterval < time.Second {
		renewInterval = time.Second
	}

	nodeID := makeNodeID()
	renewScript := redis.NewScript(renewLua)

	go func() {
		ctx := context.Background()
		isLeader := false
		lastStateLogged := time.Time{}

		logger.SysLog(fmt.Sprintf("Leader election started, node=%s, lease=%ds, renew=%s", nodeID, leaseSeconds, renewInterval))

		logState := func(msg string) {
			// Avoid log spam: at most once every 30s unless state flips
			if time.Since(lastStateLogged) >= 30*time.Second {
				logger.SysLog(msg)
				lastStateLogged = time.Now()
			}
		}

		for {
			if !isLeader {
				// Try to acquire leadership
				ok, err := client.SetNX(ctx, leaderKey, nodeID, leaseTTL).Result()
				if err != nil {
					logger.SysError(fmt.Sprintf("Leader election SetNX error (node=%s): %v", nodeID, err))
				}
				if ok {
					// We are the leader now
					if !config.IsMasterNode {
						logger.SysLog(fmt.Sprintf("Leadership acquired, node=%s", nodeID))
					}
					config.IsMasterNode = true
					isLeader = true
				} else {
					// Not leader
					if config.IsMasterNode {
						logger.SysLog(fmt.Sprintf("Leadership lost (another node holds the lease), node=%s", nodeID))
					}
					config.IsMasterNode = false
					logState(fmt.Sprintf("Follower state, waiting to acquire leadership, node=%s", nodeID))
				}
				time.Sleep(renewInterval)
				continue
			}

			// Renew lease if we still own it
			// ARGV[1]=nodeID, ARGV[2]=ttlMillis
			ttlMillis := int(leaseTTL / time.Millisecond)
			res, err := renewScript.Run(ctx, client, []string{leaderKey}, nodeID, ttlMillis).Result()
			if err != nil {
				logger.SysError(fmt.Sprintf("Leader renew error (node=%s): %v", nodeID, err))
			}

			switch v := res.(type) {
			case int64:
				if v == 1 {
					// Successfully renewed; stay leader
					logState(fmt.Sprintf("Leader state, lease renewed, node=%s", nodeID))
				} else {
					// Renew failed; demote
					isLeader = false
					if config.IsMasterNode {
						logger.SysLog(fmt.Sprintf("Leadership renewal failed, demoting to follower, node=%s", nodeID))
					}
					config.IsMasterNode = false
				}
			default:
				// Unexpected response; be conservative: demote
				isLeader = false
				if config.IsMasterNode {
					logger.SysLog(fmt.Sprintf("Leadership renewal returned unexpected result, demoting to follower, node=%s", nodeID))
				}
				config.IsMasterNode = false
			}

			time.Sleep(renewInterval)
		}
	}()
}

func makeNodeID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s-%s", host, uuid.NewString())
}
