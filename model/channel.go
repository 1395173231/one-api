package model

import (
	"context"
	"gorm.io/gorm"
	"one-api/common"
	"time"
)

type Channel struct {
	Id                 int     `json:"id"`
	Type               int     `json:"type" gorm:"default:0"`
	Key                string  `json:"key" gorm:"not null;index"`
	Status             int     `json:"status" gorm:"default:1"`
	Name               string  `json:"name" gorm:"index"`
	Weight             *uint   `json:"weight" gorm:"default:0"`
	CreatedTime        int64   `json:"created_time" gorm:"bigint"`
	TestTime           int64   `json:"test_time" gorm:"bigint"`
	ResponseTime       int     `json:"response_time"` // in milliseconds
	FullURL            string  `json:"full_url" gorm:"column:full_url;default:''"`
	BaseURL            *string `json:"base_url" gorm:"column:base_url;default:''"`
	Other              string  `json:"other"`
	Balance            float64 `json:"balance"` // in USD
	BalanceUpdatedTime int64   `json:"balance_updated_time" gorm:"bigint"`
	Models             string  `json:"models"`
	Group              string  `json:"group" gorm:"type:varchar(32);default:'default'"`
	UsedQuota          int64   `json:"used_quota" gorm:"bigint;default:0"`
	AsyncNum           int     `json:"async_num" gorm:"column:async_num;default:1"`
	ModelMapping       *string `json:"model_mapping" gorm:"type:varchar(1024);default:''"`
	Priority           *int64  `json:"priority" gorm:"bigint;default:0"`
	AutoBan            *int    `json:"auto_ban" gorm:"default:1"`
}

func GetEnableChannels() ([]*Channel, error) {
	startTime := time.Now()
	defer func() {
		common.SysLog("get enable channels took " + time.Since(startTime).String())
	}()

	if !common.RedisEnabled {
		return readChannelsFromDB()
	}
	var ctx = context.Background()
	// 分布式锁的键
	lockKey := "channel:enable:list:lock"
	// 尝试获取分布式锁
	lockSuccess, err := common.RDB.SetNX(ctx, lockKey, "1", 5*time.Second).Result()
	if err != nil {
		// 发生错误时直接从数据库读取
		return readChannelsFromDB()
	}
	if !lockSuccess {
		// 获取锁失败时直接从数据库读取
		return readChannelsFromDB()
	}
	defer func() {
		// 释放锁
		common.RDB.Del(ctx, lockKey)
	}()

	// 从Redis中获取
	val, err := common.RDB.LRange(ctx, "channel:enable:list", 0, -1).Result()
	if err != nil || len(val) == 0 {
		// 缓存中不存在，从数据库加载并更新缓存
		return readAndCacheChannels(ctx)
	}

	// 解析从缓存中获取的数据
	channels := make([]*Channel, len(val))
	for i, jsonData := range val {
		var channel Channel
		err = json.Unmarshal([]byte(jsonData), &channel)
		if err != nil {
			continue
		}
		channels[i] = &channel
	}
	return channels, nil
}

func readChannelsFromDB() ([]*Channel, error) {
	var channels []*Channel
	err := DB.Where("status = ?", common.ChannelStatusEnabled).Find(&channels).Error
	return channels, err
}

func readAndCacheChannels(ctx context.Context) ([]*Channel, error) {
	var channels []*Channel
	err := DB.Where("status = ?", common.ChannelStatusEnabled).Find(&channels).Error
	if err != nil {
		return nil, err
	}
	// 序列化channels并保存到Redis
	for _, channel := range channels {
		channelData, _ := json.Marshal(channel)
		common.RDB.LPush(ctx, "channel:enable:list", channelData)
	}
	common.RDB.Expire(ctx, "channel:enable:list", 150*time.Second)
	return channels, nil
}

func GetAllChannels(startIdx int, num int, selectAll bool, idSort bool) ([]*Channel, error) {
	var channels []*Channel
	var err error
	order := "priority desc"
	if idSort {
		order = "id desc"
	}
	if selectAll {
		err = DB.Order(order).Find(&channels).Error
	} else {
		err = DB.Order(order).Limit(num).Offset(startIdx).Omit("key").Find(&channels).Error
	}
	return channels, err
}

func SearchChannels(keyword string, group string) (channels []*Channel, err error) {
	keyCol := "`key`"
	if common.UsingPostgreSQL {
		keyCol = `"key"`
	}
	if group != "" {
		groupCol := "`group`"
		if common.UsingPostgreSQL {
			groupCol = `"group"`
		}
		err = DB.Omit("key").Where("(id = ? or name LIKE ? or "+keyCol+" = ?) and "+groupCol+" LIKE ?", common.String2Int(keyword), keyword+"%", keyword, "%"+group+"%").Find(&channels).Error
	} else {
		err = DB.Omit("key").Where("id = ? or name LIKE ? or "+keyCol+" = ?", common.String2Int(keyword), keyword+"%", keyword).Find(&channels).Error
	}
	return channels, err
}

func GetChannelById(id int, selectAll bool) (*Channel, error) {
	channel := Channel{Id: id}
	var err error = nil
	if selectAll {
		err = DB.First(&channel, "id = ?", id).Error
	} else {
		err = DB.Omit("key").First(&channel, "id = ?", id).Error
	}
	return &channel, err
}

func BatchInsertChannels(channels []Channel) error {
	var err error
	defer InitChannelCache()
	err = DB.CreateInBatches(&channels, 40).Error
	if err != nil {
		return err
	}
	for _, channel_ := range channels {
		err = channel_.AddAbilities()
		if err != nil {
			return err
		}
	}
	return nil
}

func (channel *Channel) GetPriority() int64 {
	if channel.Priority == nil {
		return 0
	}
	return *channel.Priority
}

func (channel *Channel) GetBaseURL() string {
	if channel.BaseURL == nil {
		return ""
	}
	return *channel.BaseURL
}

func (channel *Channel) GetModelMapping() string {
	if channel.ModelMapping == nil {
		return ""
	}
	return *channel.ModelMapping
}

func (channel *Channel) Insert() error {
	var err error
	err = DB.Create(channel).Error
	if err != nil {
		return err
	}
	err = channel.AddAbilities()
	return err
}

func (channel *Channel) Update() error {
	var err error
	err = DB.Model(channel).Updates(channel).Error
	if err != nil {
		return err
	}
	DB.Model(channel).First(channel, "id = ?", channel.Id)
	err = channel.UpdateAbilities()
	return err
}

func (channel *Channel) UpdateResponseTime(responseTime int64) {
	err := DB.Model(channel).Select("response_time", "test_time").Updates(Channel{
		TestTime:     common.GetTimestamp(),
		ResponseTime: int(responseTime),
	}).Error
	if err != nil {
		common.SysError("failed to update response time: " + err.Error())
	}
}

func (channel *Channel) UpdateBalance(balance float64) {
	err := DB.Model(channel).Select("balance_updated_time", "balance").Updates(Channel{
		BalanceUpdatedTime: common.GetTimestamp(),
		Balance:            balance,
	}).Error
	if err != nil {
		common.SysError("failed to update balance: " + err.Error())
	}
}

func (channel *Channel) Delete() error {
	var err error
	err = DB.Delete(channel).Error
	if err != nil {
		return err
	}
	err = channel.DeleteAbilities()
	return err
}

func UpdateChannelStatusById(id int, status int) {
	err := UpdateAbilityStatus(id, status == common.ChannelStatusEnabled)
	if err != nil {
		common.SysError("failed to update ability status: " + err.Error())
	}
	err = DB.Model(&Channel{}).Where("id = ?", id).Update("status", status).Error
	if err != nil {
		common.SysError("failed to update channel status: " + err.Error())
	}
}

func UpdateChannelUsedQuota(id int, quota int) {
	if common.BatchUpdateEnabled {
		addNewRecord(BatchUpdateTypeChannelUsedQuota, id, quota)
		return
	}
	updateChannelUsedQuota(id, quota)
}

func updateChannelUsedQuota(id int, quota int) {
	err := DB.Model(&Channel{}).Where("id = ?", id).Update("used_quota", gorm.Expr("used_quota + ?", quota)).Error
	if err != nil {
		common.SysError("failed to update channel used quota: " + err.Error())
	}
}

func DeleteChannelByStatus(status int64) (int64, error) {
	result := DB.Where("status = ?", status).Delete(&Channel{})
	return result.RowsAffected, result.Error
}

func DeleteDisabledChannel() (int64, error) {
	defer func() {
		err := UpdateAllAbilities()
		if err != nil {
			common.SysError("failed to update all abilities: " + err.Error())
		}
	}()
	result := DB.Where("status = ? or status = ?", common.ChannelStatusAutoDisabled, common.ChannelStatusManuallyDisabled).Delete(&Channel{})
	return result.RowsAffected, result.Error
}
