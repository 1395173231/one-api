package realtime

import (
	"context"
	"strings"
	"time"

	"one-api/common/config"
	"one-api/common/logger"
	rds "one-api/common/redis"
	"one-api/model"
)

// StartRealtimeSync starts Redis pub/sub listeners to refresh in-memory state immediately.
// - optionsTopic: triggers model.ReloadOptions()
// - channelsTopic: triggers model.ChannelGroup.Load()
//
// It also performs an initial warm-up load to avoid cold state on startup.
func StartRealtimeSync() {
	if !config.RedisEnabled {
		return
	}
	client := rds.GetRedisClient()
	if client == nil {
		logger.SysError("Realtime sync skipped: Redis client not initialized")
		return
	}

	// Warm-up: ensure state exists on boot
	go func() {
		// Small stagger to avoid thundering herd during simultaneous boots
		time.Sleep(500 * time.Millisecond)
		safeReloadOptions()
		safeReloadChannels()
	}()

	ctx := context.Background()
	pubsub := client.Subscribe(ctx, rds.RedisTopicOptionsSync, rds.RedisTopicChannelsSync)
	go func() {
		defer pubsub.Close()
		logger.SysLog("Realtime sync subscriber started (Redis Pub/Sub)")

		for {
			msg, err := pubsub.ReceiveMessage(ctx)
			if err != nil {
				if err == context.Canceled {
					return
				}
				logger.SysError("Realtime sync receive error: " + err.Error())
				time.Sleep(time.Second)
				continue
			}

			// Extract origin instance ID and payload; skip self-published messages
			payload := msg.Payload
			if sep := strings.Index(payload, "|"); sep > 0 {
				originID := payload[:sep]
				if originID == config.InstanceID {
					continue
				}
				payload = payload[sep+1:]
			}

			switch msg.Channel {
			case rds.RedisTopicOptionsSync:
				// Optional payload schema: "key=value" or "reload"
				if strings.TrimSpace(payload) == "" || strings.HasPrefix(payload, "reload") {
					safeReloadOptions()
				} else {
					// For now, do a full reload to keep behavior consistent
					safeReloadOptions()
				}
			case rds.RedisTopicChannelsSync:
				// Optional payload schema: "reload" / "change:{id}:{enabled}"
				// For simplicity and consistency, just reload the group.
				safeReloadChannels()
			default:
				// ignore unknown channels
			}
		}
	}()
}

func safeReloadOptions() {
	defer func() {
		if r := recover(); r != nil {
			logger.SysError("panic reloading options")
		}
	}()
	model.ReloadOptions()
}

func safeReloadChannels() {
	defer func() {
		if r := recover(); r != nil {
			logger.SysError("panic reloading channels")
		}
	}()
	model.ChannelGroup.Load()
	// Keep Pricing and ModelOwnedBy in sync like periodic SyncChannelCache
	if model.PricingInstance != nil {
		_ = model.PricingInstance.Init()
	}
	if model.ModelOwnedBysInstance != nil {
		_ = model.ModelOwnedBysInstance.Load()
	}
}
