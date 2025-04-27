package service

import (
	"encoding/json"
	"strconv"
)

// BilibiliBaseEvent 用于初步解析，提取 cmd 和 roomid，延迟解析 data/info
type BilibiliBaseEvent struct {
	Cmd     string          `json:"cmd"`
	Data    json.RawMessage `json:"data"`    // 使用 RawMessage 延迟解析 data
	Info    json.RawMessage `json:"info"`    // DANMU_MSG 使用 info
	RoomID  json.Number     `json:"roomid"`  // 有些事件 roomid 在顶层
	RawData []byte          `json:"rawdata"` // 存储原始 JSON 数据，手动填充
}

//DanmuMsgInfoArray DANMU_MSG 的 info 字段是一个数组
//info[0]: 各种属性，例如 [0,1,5,65535,16777215,0,0,"",0,"",0,0,0,0,0,0,0]
//info[1]: 弹幕内容 (string)
//info[2]: 用户信息 [uid, uname, is_admin, vip, svip, rank, mobile_verify, uname_color]
//info[3]: 勋章信息 [level, medal_name, anchor_uname, room_id, medal_color_start, medal_color_end, medal_color_border]
//info[4]: 等级信息 [ul, ul_rank, ul_color]
//info[5]: 粉丝牌信息 [fans_medal_level, fans_medal_name, fans_medal_wearing_status]
//info[6]: 老爷信息 [old_title, title]
//info[7]: 弹幕类型？ 0 普通
//info[8]: 表情包信息？ {"url": "..."}
//... 后续字段不确定

// DanmuMsgUserInfoArray DANMU_MSG 的 info[2] 用户信息数组
// [uid, uname, is_admin, vip, svip, ...]
type DanmuMsgUserInfoArray []interface{}

func (u DanmuMsgUserInfoArray) UID() json.Number {
	if len(u) > 0 {
		if num, ok := u[0].(float64); ok {
			return json.Number(strconv.FormatFloat(num, 'f', -1, 64))
		}
		if str, ok := u[0].(string); ok {
			return json.Number(str)
		}
		if num, ok := u[0].(json.Number); ok {
			return num
		}
	}
	return ""
}

func (u DanmuMsgUserInfoArray) Uname() string {
	if len(u) > 1 {
		if str, ok := u[1].(string); ok {
			return str
		}
	}
	return ""
}

// InteractWordData INTERACT_WORD data 字段结构
type InteractWordData struct {
	// Contribution struct { Grade int `json:"grade"` } `json:"contribution"` // 忽略贡献信息
	// DMScore  int         `json:"dmscore"` // 忽略
	FansMedal struct {
		// AnchorRoomID     int    `json:"anchor_roomid"`
		GuardLevel int `json:"guard_level"`
		// IconID           int    `json:"icon_id"`
		// IsLighted        int    `json:"is_lighted"`
		// MedalColor       int    `json:"medal_color"`
		// MedalColorBorder int    `json:"medal_color_border"`
		// MedalColorEnd    int    `json:"medal_color_end"`
		// MedalColorStart  int    `json:"medal_color_start"`
		MedalLevel int    `json:"medal_level"`
		MedalName  string `json:"medal_name"`
		// Score            int    `json:"score"`
		// Special          string `json:"special"`
		// TargetID         int    `json:"target_id"`
	} `json:"fans_medal"`
	// Identities []int       `json:"identities"` // 忽略身份标识
	// IsSpread   int         `json:"is_spread"` // 忽略传播信息
	MsgType int `json:"msg_type"` // 1: 进入, 2: 关注
	// PrivilegeType int      `json:"privilege_type"` // 忽略权限类型
	RoomID json.Number `json:"roomid"`
	// Score      int64       `json:"score"` // 忽略积分
	// SpreadDesc string      `json:"spread_desc"`
	// SpreadInfo string      `json:"spread_info"`
	// TailIcon   int         `json:"tail_icon"` // 忽略尾部图标
	Timestamp int64 `json:"timestamp"` // 事件发生时间戳（秒）
	// TriggerTime int64      `json:"trigger_time"` // 忽略触发时间
	UID   json.Number `json:"uid"`
	Uname string      `json:"uname"`
	// UnameColor string      `json:"uname_color"` // 忽略用户名颜色
}

// SendGiftData SEND_GIFT data 字段结构
type SendGiftData struct {
	Action string `json:"action"` // "喂食"
	// BatchComboID   string      `json:"batch_combo_id"` // 忽略连击信息
	// BatchComboSend json.RawMessage `json:"batch_combo_send"`
	// BeatID           string      `json:"beatId"` // 忽略节奏风暴 ID
	// BizSource        string      `json:"biz_source"` // 忽略业务来源
	// BlindGift        json.RawMessage `json:"blind_gift"` // 忽略盲盒信息
	// BroadcastID      int         `json:"broadcast_id"` // 忽略广播 ID
	CoinType string `json:"coin_type"` // "gold", "silver"
	// ComboResourcesID int         `json:"combo_resources_id"` // 忽略连击资源 ID
	// ComboSend        json.RawMessage `json:"combo_send"` // 忽略单次连击信息
	// ComboStayTime    int         `json:"combo_stay_time"` // 忽略连击停留时间
	// ComboTotalCoin   int         `json:"combo_total_coin"` // 忽略连击总价值
	// CritProb         int         `json:"crit_prob"` // 忽略暴击概率
	// Demarcation      int         `json:"demarcation"` // 忽略分界线
	// DiscountPrice    int         `json:"discount_price"` // 忽略折扣价
	// DmScore          int         `json:"dmscore"` // 忽略弹幕积分
	// Draw             int         `json:"draw"` // 忽略抽奖信息
	// Effect           int         `json:"effect"` // 忽略特效
	// EffectBlock      int         `json:"effect_block"` // 忽略特效屏蔽
	Face string `json:"face"` // 用户头像 URL
	// FansMedal        json.RawMessage `json:"fans_medal"` // 忽略粉丝勋章（在 medal_info 中）
	GiftID   int    `json:"giftId"`
	GiftName string `json:"giftName"`
	// GiftType         int         `json:"giftType"` // 忽略礼物类型
	// Gold             int         `json:"gold"` // 忽略金瓜子数（在 total_coin 中）
	GuardLevel int `json:"guard_level"` // 大航海等级
	// IsFirst          bool        `json:"is_first"` // 忽略是否首次
	// IsSpecialBatch   int         `json:"is_special_batch"` // 忽略是否特殊批次
	// Magnification    float64     `json:"magnification"` // 忽略放大倍数
	MedalInfo struct {
		// AnchorRoomID     int    `json:"anchor_roomid"`
		// AnchorUname      string `json:"anchor_uname"`
		GuardLevel int `json:"guard_level"`
		// IconID           int    `json:"icon_id"`
		// IsLighted        int    `json:"is_lighted"`
		// MedalColor       int    `json:"medal_color"`
		// MedalColorBorder int    `json:"medal_color_border"`
		// MedalColorEnd    int    `json:"medal_color_end"`
		// MedalColorStart  int    `json:"medal_color_start"`
		MedalLevel int    `json:"medal_level"`
		MedalName  string `json:"medal_name"`
		// Special          string `json:"special"`
		// TargetID         int    `json:"target_id"`
	} `json:"medal_info"`
	// NameColor        string      `json:"name_color"` // 忽略名称颜色
	Num int `json:"num"` // 礼物数量
	// OriginalGiftName string      `json:"original_gift_name"` // 忽略原始礼物名
	// Price            int         `json:"price"` // 忽略单价（在 total_coin 中）
	// Rcost            int         `json:"rcost"` // 忽略房间货币花费
	// Remain           int         `json:"remain"` // 忽略剩余数量
	// Rnd              string      `json:"rnd"` // 忽略随机数
	// SendMaster       json.RawMessage `json:"send_master"` // 忽略发送者信息
	// Silver           int         `json:"silver"` // 忽略银瓜子数（在 total_coin 中）
	// Super            int         `json:"super"` // 忽略 Super 礼物标记
	// SuperBatchGiftNum int        `json:"super_batch_gift_num"` // 忽略 Super 批次数量
	// SuperGiftNum     int         `json:"super_gift_num"` // 忽略 Super 礼物数量
	// SvgaBlock        int         `json:"svga_block"` // 忽略 SVGA 屏蔽
	// TagImage         string      `json:"tag_image"` // 忽略标签图片
	Tid       string `json:"tid"`       // 交易/事件 ID
	Timestamp int64  `json:"timestamp"` // 事件时间戳（秒）
	// TopList          json.RawMessage `json:"top_list"` // 忽略 Top 列表
	TotalCoin int64       `json:"total_coin"` // 礼物总价值（金/银瓜子）
	UID       json.Number `json:"uid"`        // 用户 ID
	Uname     string      `json:"uname"`      // 用户名
}

// GuardBuyData GUARD_BUY data 字段结构
type GuardBuyData struct {
	// GiftID     int         `json:"gift_id"` // 忽略礼物 ID
	// GiftName   string      `json:"gift_name"` // 忽略礼物名
	GuardLevel int   `json:"guard_level"` // 3:舰长, 2:提督, 1:总督
	Num        int   `json:"num"`         // 购买数量
	Price      int64 `json:"price"`       // 价格（金瓜子）
	StartTime  int64 `json:"start_time"`  // 开始时间戳（秒）
	// EndTime    int64       `json:"end_time"` // 忽略结束时间
	UID      json.Number `json:"uid"`      // 用户 ID
	Username string      `json:"username"` // 用户名
	// TargetID? RoomID? - 这些信息通常在顶层或需要关联获取
}

// SuperChatMessageData SUPER_CHAT_MESSAGE data 字段结构
type SuperChatMessageData struct {
	// BackgroundColor      string      `json:"background_color"` // 忽略背景色
	// BackgroundColorEnd   string      `json:"background_color_end"`
	// BackgroundColorStart string      `json:"background_color_start"`
	// BackgroundIcon       string      `json:"background_icon"`
	// BackgroundImage      string      `json:"background_image"`
	// BackgroundPriceColor string      `json:"background_price_color"`
	// ColorPoint           float64     `json:"color_point"`
	// DmScore              int         `json:"dmscore"`
	EndTime int64  `json:"end_time"` // 结束时间戳（秒）
	Face    string `json:"face"`     // 用户头像
	// FaceFrame            string      `json:"face_frame"` // 忽略头像框
	Gift struct {
		GiftID int `json:"gift_id"` // 12000
		// GiftName string `json:"gift_name"` // "醒目留言"
		Num int `json:"num"` // 1
	} `json:"gift"`
	GuardLevel int         `json:"guard_level"` // 大航海等级
	ID         json.Number `json:"id"`          // SC 消息 ID
	// IsRanked         int         `json:"is_ranked"` // 忽略是否计入排行
	// IsSendAudit      int         `json:"is_send_audit"` // 忽略是否审核
	MedalInfo struct {
		// AnchorRoomID     int    `json:"anchor_roomid"`
		// AnchorUname      string `json:"anchor_uname"`
		GuardLevel int `json:"guard_level"`
		// IconID           int    `json:"icon_id"`
		// IsLighted        int    `json:"is_lighted"`
		// MedalColor       string `json:"medal_color"`
		// MedalColorBorder int    `json:"medal_color_border"`
		// MedalColorEnd    int    `json:"medal_color_end"`
		// MedalColorStart  int    `json:"medal_color_start"`
		MedalLevel int    `json:"medal_level"`
		MedalName  string `json:"medal_name"`
		// Special          string `json:"special"`
		// TargetID         int    `json:"target_id"`
	} `json:"medal_info"`
	Message string `json:"message"` // SC 内容
	// MessageFontColor string      `json:"message_font_color"` // 忽略字体颜色
	// MessageTrans     string      `json:"message_trans"` // 忽略翻译
	Price int `json:"price"` // 价格 (CNY)
	// Rate             int         `json:"rate"` // 忽略汇率
	RoomID    json.Number `json:"roomid"`     // 房间 ID
	StartTime int64       `json:"start_time"` // 开始时间戳（秒）
	Time      int         `json:"time"`       // 持续时间（秒）
	// Token            string      `json:"token"` // 忽略 Token
	// TransMark        int         `json:"trans_mark"` // 忽略翻译标记
	// Ts               int64       `json:"ts"` // 忽略另一个时间戳
	UID      json.Number `json:"uid"` // 用户 ID
	UserInfo struct {
		Face string `json:"face"`
		// FaceFrame  string `json:"face_frame"`
		GuardLevel int    `json:"guard_level"`
		Uname      string `json:"uname"`
		// UserLevel  int    `json:"user_level"`
	} `json:"user_info"`
}

// WatchedChangeData WATCHED_CHANGE data 字段结构
type WatchedChangeData struct {
	Num int64 `json:"num"` // 当前看过人数
	// TextSmall string `json:"text_small"` // 忽略小文本
	TextLarge string `json:"text_large"` // "1.7万人看过"
}

// LikeInfoV3UpdateData LIKE_INFO_V3_UPDATE data 字段结构
type LikeInfoV3UpdateData struct {
	ClickCount int64 `json:"click_count"` // 点赞计数
	// LikeText   string `json:"like_text"` // 忽略点赞文本
}

// OnlineRankV2Data ONLINE_RANK_V2 data 字段结构
type OnlineRankV2Data struct {
	List []OnlineRankUser `json:"list"`
	// RankType  string           `json:"rank_type"` // 忽略排行类型
	// Timestamp int64            `json:"timestamp"` // 忽略时间戳
}

// OnlineRankUser ONLINE_RANK_V2 list 字段中的用户结构
type OnlineRankUser struct {
	UID   json.Number `json:"uid"`
	Face  string      `json:"face"`
	Score string      `json:"score"` // 注意是字符串 "114514"
	Uname string      `json:"uname"`
	Rank  int         `json:"rank"`
	// GuardLevel int       `json:"guard_level"` // 忽略大航海等级
	// ... 可能还有其他字段
}

// OnlineRankCountData ONLINE_RANK_COUNT data 字段结构
type OnlineRankCountData struct {
	Count int64 `json:"count"` // 高能榜总人数
}

// RoomChangeData represents the data field for ROOM_CHANGE events.
type RoomChangeData struct {
	Title          string `json:"title"`
	AreaName       string `json:"area_name"`        // Sub-area name
	ParentAreaName string `json:"parent_area_name"` // Parent area name
	// ... other fields like area_id, parent_area_id
}
