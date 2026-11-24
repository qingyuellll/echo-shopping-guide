// Package ticketing 提供 Echo 在线票务系统的只读数据访问。
//
// 数据来源有两种，按可用性自动选择：
//  1. HTTP：TICKETING_API_BASE 指向 Echo 后端（例如 http://127.0.0.1:8081）。
//     走接口的好处是上架状态、秒杀状态、金额换算都由 Echo 的业务层判定，
//     导购拿到的就是 Echo 对外承诺的数据口径。
//  2. MySQL：TICKETING_DSN 指向 Echo 的库（echo），只读。
//     用于 Echo 后端未启动时的降级，保证导购能力仍然可用。
//
// 本包只读：不写入、不改表结构，Echo 项目无需任何改动。
package ticketing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// Echo API 是默认数据源；数据库降级必须显式配置只读账号。
const defaultAPIBaseURL = "http://127.0.0.1:8081"

type Client struct {
	feedMu          sync.Mutex
	feedSnapshot    *ReviewReport
	feedExpires     time.Time
	catalogMu       sync.Mutex
	catalogSnapshot *CatalogSnapshot
	catalogExpires  time.Time
	db              *sql.DB
	apiBase         string
	http            *http.Client
	err             error // 仅保存初始化错误；请求期间不修改，支持并发读取。
}

func NewFromEnv() *Client {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("TICKETING_API_BASE")), "/")
	if base == "" {
		base = defaultAPIBaseURL
	}
	client := &Client{apiBase: base, http: &http.Client{Timeout: 8 * time.Second}}
	if dsn := strings.TrimSpace(os.Getenv("TICKETING_DSN")); dsn != "" {
		client.db, client.err = openDB(dsn)
	}
	return client
}

func openDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open ticketing db: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping ticketing db: %w", err)
	}
	return db, nil
}

// Available 报告是否存在可用数据通路。
func (c *Client) Available() bool {
	return c != nil && (c.db != nil || c.apiBase != "")
}

// Err 返回初始化阶段的错误，便于启动日志排查。
func (c *Client) Err() error {
	if c == nil {
		return fmt.Errorf("ticketing client not initialized")
	}
	return c.err
}

// Close 释放连接池。
func (c *Client) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	return c.db.Close()
}

// Show 对应 tb_show，是一场演出的摘要信息。
type Show struct {
	PosterURL      string        `json:"poster_url"`
	Lifecycle      string        `json:"lifecycle"`
	Rating         RatingSummary `json:"rating"`
	MinPrice       *float64      `json:"min_price"`
	MaxPrice       *float64      `json:"max_price"`
	AvailableStock int           `json:"available_stock"`
	TicketsKnown   bool          `json:"tickets_known"`
	HasSeckill     bool          `json:"has_seckill"`
	DetailURL      string        `json:"detail_url"`
	ID             int64         `json:"show_id,string"`
	Name           string        `json:"name"`
	TypeName       string        `json:"type_name"`
	Area           string        `json:"area"`
	Address        string        `json:"address"`
	AvgPrice       float64       `json:"avg_price"`
	Score          float64       `json:"score"`
	Sold           int           `json:"sold"`
	Comments       int           `json:"comments"`
	OpenTime       string        `json:"open_time"`
	EndTime        string        `json:"end_time"`
	Description    string        `json:"description"`
	TicketCount    int           `json:"ticket_tier_count"`
}

// TicketTier 对应 tb_ticket，是一场演出下的一种票档：
// 同一场演出可以有「一楼 A 区 680 元」「二楼看台 280 元」等多个票档，
// 各自独立定价、独立余票、独立退改规则，也可以单独设置秒杀。
type TicketTier struct {
	CheckoutURL   string  `json:"checkout_url"`
	DetailURL     string  `json:"detail_url"`
	IsSeckill     bool    `json:"is_seckill"`
	StockSource   string  `json:"stock_source"`
	ID            int64   `json:"ticket_id,string"`
	ShowID        int64   `json:"show_id,string"`
	ShowName      string  `json:"show_name,omitempty"`
	Title         string  `json:"title"`
	SubTitle      string  `json:"sub_title"`
	Rules         string  `json:"rules"`
	Price         float64 `json:"price"`
	OriginalPrice float64 `json:"original_price"`
	Stock         int     `json:"stock"`
	TotalStock    int     `json:"total_stock"`
	SoldCount     int     `json:"sold_count"`
	HasSeats      bool    `json:"has_seats"`
	SoldOut       bool    `json:"sold_out"`
	OnSale        bool    `json:"on_sale"`
	SeckillActive bool    `json:"seckill_active"`
	SeckillStock  int     `json:"seckill_stock"`
	SeckillBegin  string  `json:"seckill_begin,omitempty"`
	SeckillEnd    string  `json:"seckill_end,omitempty"`
}

// SearchShows 按关键词检索在售演出，关键词为空时返回按销量排序的热门演出。
func (c *Client) SearchShows(ctx context.Context, keyword string, limit int) ([]Show, error) {
	if limit <= 0 || limit > 20 {
		limit = 5
	}
	keyword = strings.TrimSpace(keyword)

	if c != nil && c.apiBase != "" {
		shows, err := c.searchShowsViaAPI(ctx, keyword, limit)
		if err == nil && (len(shows) > 0 || c.db == nil) {
			return shows, nil
		}
	}
	shows, err := c.searchShowsViaDB(ctx, keyword, limit)
	if err != nil {
		return nil, err
	}
	return shows, nil
}

// ShowDetail 读取单场演出详情。
func (c *Client) ShowDetail(ctx context.Context, showID int64) (Show, error) {
	if c != nil && c.apiBase != "" {
		show, err := c.showDetailViaAPI(ctx, showID)
		if err == nil {
			return show, nil
		}
	}
	return c.showDetailViaDB(ctx, showID)
}

// ListTickets 读取某场演出在售票档，含余票、售罄、秒杀状态。
func (c *Client) ListTickets(ctx context.Context, showID int64) ([]TicketTier, error) {
	if c != nil && c.apiBase != "" {
		tiers, err := c.listTicketsViaAPI(ctx, showID)
		if err == nil {
			return tiers, nil
		}
	}
	return c.listTicketsViaDB(ctx, showID)
}

// ListSeckillTickets 列出当前配置了秒杀的票档。
func (c *Client) ListSeckillTickets(ctx context.Context, limit int) ([]TicketTier, error) {
	if limit <= 0 || limit > 20 {
		limit = 5
	}
	if c != nil && c.apiBase != "" {
		tiers, err := c.listSeckillViaAPI(ctx, limit)
		if err == nil {
			return tiers, nil
		}
	}
	return c.listSeckillViaDB(ctx, limit)
}

// ---------- HTTP 通路 ----------

type apiEnvelope[T any] struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

type wireString string

func (value *wireString) UnmarshalJSON(raw []byte) error {
	if string(raw) == "null" {
		*value = ""
		return nil
	}
	var text string
	if len(raw) > 0 && raw[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			return err
		}
	} else {
		var number json.Number
		if err := json.Unmarshal(raw, &number); err != nil {
			return err
		}
		text = number.String()
	}
	*value = wireString(text)
	return nil
}

type apiShow struct {
	Images         string     `json:"images"`
	TypeID         wireString `json:"typeId"`
	TypeName       string     `json:"typeName"`
	Status         *int       `json:"status"`
	ID             wireString `json:"id"`
	Name           string     `json:"name"`
	Area           string     `json:"area"`
	Address        string     `json:"address"`
	AvgPrice       wireString `json:"avgPrice"`
	Sold           int        `json:"sold"`
	Comments       int        `json:"comments"`
	Score          float64    `json:"score"`
	OpenTime       string     `json:"openTime"`
	EndTime        string     `json:"endTime"`
	Description    string     `json:"description"`
	FormattedPrice string     `json:"formattedPrice"`
}

type apiShowPage struct {
	Records []apiShow  `json:"records"`
	Total   wireString `json:"total"`
}

type apiTicket struct {
	SubTitle          string     `json:"subTitle"`
	Rules             string     `json:"rules"`
	TotalStock        *int       `json:"totalStock"`
	SeckillStock      *int       `json:"seckillStock"`
	BeginTime         string     `json:"beginTime"`
	EndTime           string     `json:"endTime"`
	SeckillBeginTime  string     `json:"seckillBeginTime"`
	SeckillEndTime    string     `json:"seckillEndTime"`
	Type              int        `json:"type"`
	Status            *int       `json:"status"`
	ID                wireString `json:"id"`
	ShowID            wireString `json:"showId"`
	Title             string     `json:"title"`
	PayValueInYuan    float64    `json:"payValueInYuan"`
	ActualValueInYuan float64    `json:"actualValueInYuan"`
	Stock             int        `json:"stock"`
	RemainingStock    *int       `json:"remainingStock"`
	SoldCount         int        `json:"soldCount"`
	HasSeats          bool       `json:"hasSeats"`
	SeckillTicket     bool       `json:"seckillTicket"`
	IsSeckill         bool       `json:"isSeckill"`
	SoldOut           bool       `json:"soldOut"`
	OnSale            bool       `json:"onSale"`
}

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, target any) error {
	endpoint := c.apiBase + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build request %s: %w", path, err)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("call %s: %w", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("call %s: status %d", path, response.StatusCode)
	}
	if len(body) > 8<<20 {
		return fmt.Errorf("response %s exceeds 8 MiB", path)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// HTTP 搜索覆盖名称、类型、城市、场馆与简介，无需开放 Echo 数据库。
func (c *Client) searchShowsViaAPI(ctx context.Context, keyword string, limit int) ([]Show, error) {
	type showType struct {
		ID   wireString `json:"id"`
		Name string     `json:"name"`
	}
	var types apiEnvelope[[]showType]
	_ = c.getJSON(ctx, "/show-type/list", nil, &types)
	typeNames := map[string]string{}
	for _, item := range types.Data {
		typeNames[string(item.ID)] = item.Name
	}
	query := url.Values{"size": {"20"}, "sortBy": {"sold"}, "sortOrder": {"desc"}}
	// /show/of/name 支持分页；/show/search 固定每页 20 条，不能用 size 控制。
	shows := make([]Show, 0)
	needle := strings.ToLower(strings.TrimSpace(keyword))
	for page := 1; page <= 25; page++ {
		query.Set("current", strconv.Itoa(page))
		var envelope apiEnvelope[apiShowPage]
		if err := c.getJSON(ctx, "/show/of/name", query, &envelope); err != nil {
			return nil, err
		}
		if !envelope.Success {
			return nil, fmt.Errorf("show search rejected: %s", envelope.Message)
		}
		for _, record := range envelope.Data.Records {
			if record.TypeName == "" {
				record.TypeName = typeNames[string(record.TypeID)]
			}
			show := record.toShow()
			if (record.Status != nil && *record.Status != 1) || showEnded(show.EndTime) {
				continue
			}
			haystack := strings.ToLower(strings.Join([]string{show.Name, show.TypeName, show.Area, show.Address, record.Description}, " "))
			if needle == "" || strings.Contains(haystack, needle) {
				shows = append(shows, show)
			}
		}
		if len(envelope.Data.Records) < 20 {
			break
		}
		if total := parseInt64(envelope.Data.Total); total > 0 && int64(page*20) >= total {
			break
		}
	}
	sort.SliceStable(shows, func(i, j int) bool { return shows[i].Sold > shows[j].Sold })
	if len(shows) > limit {
		shows = shows[:limit]
	}
	return shows, nil
}

func (c *Client) showDetailViaAPI(ctx context.Context, showID int64) (Show, error) {
	var envelope apiEnvelope[apiShow]
	if err := c.getJSON(ctx, "/show/"+strconv.FormatInt(showID, 10), nil, &envelope); err != nil {
		return Show{}, err
	}
	if !envelope.Success {
		return Show{}, fmt.Errorf("show detail rejected: %s", envelope.Message)
	}
	if strings.TrimSpace(string(envelope.Data.ID)) == "" {
		return Show{}, fmt.Errorf("show %d not found", showID)
	}
	show := envelope.Data.toShow()
	if envelope.Data.Status != nil && *envelope.Data.Status != 1 {
		return Show{}, fmt.Errorf("show %d is not on sale", showID)
	}
	return show, nil
}

func (c *Client) listTicketsViaAPI(ctx context.Context, showID int64) ([]TicketTier, error) {
	var envelope apiEnvelope[[]apiTicket]
	if err := c.getJSON(ctx, "/ticket/list/"+strconv.FormatInt(showID, 10), nil, &envelope); err != nil {
		return nil, err
	}
	if !envelope.Success {
		return nil, fmt.Errorf("ticket list rejected: %s", envelope.Message)
	}
	show, err := c.showDetailViaAPI(ctx, showID)
	if err != nil {
		return nil, err
	}
	tiers := make([]TicketTier, 0, len(envelope.Data))
	for _, record := range envelope.Data {
		if parseInt64(record.ShowID) == 0 {
			record.ShowID = wireString(strconv.FormatInt(showID, 10))
		}
		tier := record.toTicketTier(show.Name)
		if tier.OnSale {
			tiers = append(tiers, tier)
		}
	}
	return tiers, nil
}

// listSeckillViaAPI 借助演出列表 + 票档列表筛出秒杀票。
// Echo 未提供秒杀票的独立查询接口，因此按演出逐个取票档后过滤，
// 演出一场一场查的代价可接受，因为 limit 很小。
func (c *Client) listSeckillViaAPI(ctx context.Context, limit int) ([]TicketTier, error) {
	shows, err := c.searchShowsViaAPI(ctx, "", limit*3)
	if err != nil {
		return nil, err
	}
	result := make([]TicketTier, 0, limit)
	for _, show := range shows {
		var envelope apiEnvelope[[]apiTicket]
		if err := c.getJSON(ctx, "/ticket/list/"+strconv.FormatInt(show.ID, 10), nil, &envelope); err != nil {
			return nil, err
		}
		if !envelope.Success {
			return nil, fmt.Errorf("ticket query rejected: %s", envelope.Message)
		}
		for _, record := range envelope.Data {
			if !record.SeckillTicket && !record.IsSeckill && record.Type != 1 {
				continue
			}
			tier := record.toTicketTier(show.Name)
			if !tier.OnSale || seckillEnded(tier.SeckillEnd) {
				continue
			}
			result = append(result, tier)
			if len(result) >= limit {
				return result, nil
			}
		}
	}
	return result, nil
}

// standardPlaceName keeps filter and search values intact while showing the
// standard name for Hong Kong, Macao and Taiwan.
func standardPlaceName(value string) string {
	trimmed := strings.TrimSpace(value)
	for _, pair := range [][2]string{
		{"香港特别行政区", "中国香港"}, {"香港", "中国香港"},
		{"澳门特别行政区", "中国澳门"}, {"澳门", "中国澳门"},
		{"台湾省", "中国台湾"}, {"台湾", "中国台湾"},
	} {
		if strings.HasPrefix(trimmed, pair[0]) {
			return pair[1] + strings.TrimPrefix(trimmed, pair[0])
		}
	}
	return value
}

func (r apiShow) toShow() Show {
	return Show{
		PosterURL:   resolveImage(defaultAPIBaseURL, r.Images),
		Lifecycle:   Lifecycle(Show{OpenTime: r.OpenTime, EndTime: r.EndTime}),
		ID:          parseInt64(r.ID),
		DetailURL:   ShowURL(parseInt64(r.ID)),
		TypeName:    r.TypeName,
		Name:        r.Name,
		Area:        standardPlaceName(r.Area),
		Address:     standardPlaceName(r.Address),
		AvgPrice:    parseCentsToYuan(r.AvgPrice),
		Score:       r.Score,
		Sold:        r.Sold,
		Comments:    r.Comments,
		OpenTime:    normalizeAPITime(r.OpenTime),
		EndTime:     normalizeAPITime(r.EndTime),
		Description: truncateRunes(r.Description, 200),
	}
}

func (r apiTicket) toTicketTier(showName string) TicketTier {
	isSeckill := r.SeckillTicket || r.IsSeckill || r.Type == 1
	stock := r.Stock
	if r.RemainingStock != nil {
		stock = *r.RemainingStock
	}
	if isSeckill && r.SeckillStock != nil {
		stock = *r.SeckillStock
	}
	if stock < 0 {
		stock = 0
	}
	total := r.Stock
	if r.TotalStock != nil {
		total = *r.TotalStock
	}
	onSale := r.OnSale
	if r.Status != nil {
		onSale = *r.Status == 1
	}
	begin, end := r.BeginTime, r.EndTime
	if begin == "" {
		begin = r.SeckillBeginTime
	}
	if end == "" {
		end = r.SeckillEndTime
	}
	begin, end = normalizeAPITime(begin), normalizeAPITime(end)
	return TicketTier{
		CheckoutURL: strings.TrimSuffix(ShowURL(0), "/show/0") + "/guide/checkout/" + string(r.ID),
		ID:          parseInt64(r.ID), ShowID: parseInt64(r.ShowID), ShowName: showName,
		DetailURL: ShowURL(parseInt64(r.ShowID)), Title: r.Title, SubTitle: r.SubTitle,
		Rules: plainText(r.Rules), Price: r.PayValueInYuan, OriginalPrice: r.ActualValueInYuan,
		Stock: stock, TotalStock: total, SoldCount: r.SoldCount, HasSeats: r.HasSeats,
		SoldOut: stock <= 0, OnSale: onSale, IsSeckill: isSeckill,
		SeckillActive: isSeckill && onSale && stock > 0 && seckillRunning(begin, end),
		SeckillStock:  stock, SeckillBegin: begin, SeckillEnd: end, StockSource: "echo_api",
	}
}

// ShowURL 只导航到 Echo，购票、选座与支付继续由 Echo 自己处理。
func ShowURL(showID int64) string {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("ECHO_WEB_BASE")), "/")
	if base == "" {
		base = "http://127.0.0.1:3002"
	}
	return base + "/show/" + strconv.FormatInt(showID, 10)
}

func parseBusinessTime(value string) (time.Time, bool) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		location = time.FixedZone("CST", 8*60*60)
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if parsed, err := time.ParseInLocation(layout, strings.TrimSpace(value), location); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func showEnded(value string) bool {
	end, ok := parseBusinessTime(value)
	return ok && !time.Now().Before(end)
}
func seckillEnded(value string) bool { return showEnded(value) }
func seckillRunning(begin, end string) bool {
	startTime, startOK := parseBusinessTime(begin)
	endTime, endOK := parseBusinessTime(end)
	now := time.Now()
	return startOK && endOK && !now.Before(startTime) && now.Before(endTime)
}

// ---------- MySQL 通路 ----------

func (c *Client) requireDB() error {
	if c == nil || c.db == nil {
		return fmt.Errorf("ticketing db unavailable")
	}
	return nil
}

func (c *Client) searchShowsViaDB(ctx context.Context, keyword string, limit int) ([]Show, error) {
	if err := c.requireDB(); err != nil {
		return nil, err
	}
	const baseSelect = `
SELECT s.id, s.name, IFNULL(t.name, ''), IFNULL(s.area, ''), s.address,
       IFNULL(s.avg_price, 0), IFNULL(s.score, 0), IFNULL(s.sold, 0), IFNULL(s.comments, 0),
       s.open_time, s.end_time, IFNULL(s.description, ''),
       (SELECT COUNT(*) FROM tb_ticket tk WHERE tk.show_id = s.id AND tk.status = 1)
FROM tb_show s
LEFT JOIN tb_show_type t ON t.id = s.type_id
WHERE s.status = 1 AND (s.end_time IS NULL OR s.end_time > NOW())`

	var (
		rows *sql.Rows
		err  error
	)
	if keyword == "" {
		rows, err = c.db.QueryContext(ctx, baseSelect+`
ORDER BY s.sold DESC, s.score DESC
LIMIT ?`, limit)
	} else {
		like := "%" + keyword + "%"
		rows, err = c.db.QueryContext(ctx, baseSelect+`
  AND (s.name LIKE ? OR IFNULL(s.area,'') LIKE ? OR IFNULL(t.name,'') LIKE ? OR IFNULL(s.description,'') LIKE ? OR s.address LIKE ?)
ORDER BY s.sold DESC, s.score DESC
LIMIT ?`, like, like, like, like, like, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("query shows: %w", err)
	}
	defer rows.Close()
	return scanShows(rows)
}

func (c *Client) showDetailViaDB(ctx context.Context, showID int64) (Show, error) {
	if err := c.requireDB(); err != nil {
		return Show{}, err
	}
	const query = `
SELECT s.id, s.name, IFNULL(t.name, ''), IFNULL(s.area, ''), s.address,
       IFNULL(s.avg_price, 0), IFNULL(s.score, 0), IFNULL(s.sold, 0), IFNULL(s.comments, 0),
       s.open_time, s.end_time, IFNULL(s.description, ''),
       (SELECT COUNT(*) FROM tb_ticket tk WHERE tk.show_id = s.id AND tk.status = 1)
FROM tb_show s
LEFT JOIN tb_show_type t ON t.id = s.type_id
WHERE s.id = ? AND s.status = 1 AND (s.end_time IS NULL OR s.end_time > NOW())`
	rows, err := c.db.QueryContext(ctx, query, showID)
	if err != nil {
		return Show{}, fmt.Errorf("query show detail: %w", err)
	}
	defer rows.Close()
	shows, err := scanShows(rows)
	if err != nil {
		return Show{}, err
	}
	if len(shows) == 0 {
		return Show{}, fmt.Errorf("show %d not found", showID)
	}
	return shows[0], nil
}

func (c *Client) listTicketsViaDB(ctx context.Context, showID int64) ([]TicketTier, error) {
	if err := c.requireDB(); err != nil {
		return nil, err
	}
	const query = `
SELECT tk.id, tk.show_id, s.name, tk.title, IFNULL(tk.sub_title, ''), IFNULL(tk.rules, ''),
       tk.pay_value, tk.actual_value, GREATEST(0, IFNULL(tk.stock, 0) - (SELECT COUNT(*) FROM tb_ticket_order o WHERE o.ticket_id = tk.id AND o.status IN (2,3))),
       IFNULL(tk.total_stock, tk.stock), (SELECT COUNT(*) FROM tb_ticket_order o WHERE o.ticket_id = tk.id AND o.status IN (2,3)), tk.has_seats,
       IFNULL(sk.stock, 0), sk.begin_time, sk.end_time
FROM tb_ticket tk
JOIN tb_show s ON s.id = tk.show_id
LEFT JOIN tb_seckill_ticket sk ON sk.ticket_id = tk.id
WHERE tk.show_id = ? AND tk.status = 1 AND s.status = 1 AND (s.end_time IS NULL OR s.end_time > NOW())
ORDER BY tk.pay_value ASC`
	rows, err := c.db.QueryContext(ctx, query, showID)
	if err != nil {
		return nil, fmt.Errorf("query tickets: %w", err)
	}
	defer rows.Close()
	return scanTickets(rows)
}

func (c *Client) listSeckillViaDB(ctx context.Context, limit int) ([]TicketTier, error) {
	if err := c.requireDB(); err != nil {
		return nil, err
	}
	const query = `
SELECT tk.id, tk.show_id, s.name, tk.title, IFNULL(tk.sub_title, ''), IFNULL(tk.rules, ''),
       tk.pay_value, tk.actual_value, GREATEST(0, IFNULL(tk.stock, 0) - (SELECT COUNT(*) FROM tb_ticket_order o WHERE o.ticket_id = tk.id AND o.status IN (2,3))),
       IFNULL(tk.total_stock, tk.stock), (SELECT COUNT(*) FROM tb_ticket_order o WHERE o.ticket_id = tk.id AND o.status IN (2,3)), tk.has_seats,
       IFNULL(sk.stock, 0), sk.begin_time, sk.end_time
FROM tb_seckill_ticket sk
JOIN tb_ticket tk ON tk.id = sk.ticket_id
JOIN tb_show s ON s.id = tk.show_id
WHERE tk.status = 1 AND s.status = 1 AND sk.end_time > NOW() AND (s.end_time IS NULL OR s.end_time > NOW())
ORDER BY sk.begin_time ASC
LIMIT ?`
	rows, err := c.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("query seckill tickets: %w", err)
	}
	defer rows.Close()
	return scanTickets(rows)
}

func scanShows(rows *sql.Rows) ([]Show, error) {
	items := make([]Show, 0)
	for rows.Next() {
		var (
			item     Show
			openTime sql.NullTime
			endTime  sql.NullTime
		)
		if err := rows.Scan(
			&item.ID, &item.Name, &item.TypeName, &item.Area, &item.Address,
			&item.AvgPrice, &item.Score, &item.Sold, &item.Comments,
			&openTime, &endTime, &item.Description, &item.TicketCount,
		); err != nil {
			return nil, fmt.Errorf("scan show: %w", err)
		}
		item.AvgPrice /= 100
		item.DetailURL = ShowURL(item.ID)
		item.OpenTime = formatNullTime(openTime)
		item.EndTime = formatNullTime(endTime)
		item.Description = truncateRunes(item.Description, 200)
		items = append(items, item)
	}
	return items, rows.Err()
}

func scanTickets(rows *sql.Rows) ([]TicketTier, error) {
	items := make([]TicketTier, 0)
	for rows.Next() {
		var (
			item      TicketTier
			payValue  int64
			actualVal int64
			hasSeats  bool
			secStock  int
			secBegin  sql.NullTime
			secEnd    sql.NullTime
		)
		if err := rows.Scan(
			&item.ID, &item.ShowID, &item.ShowName, &item.Title, &item.SubTitle, &item.Rules,
			&payValue, &actualVal, &item.Stock, &item.TotalStock, &item.SoldCount, &hasSeats,
			&secStock, &secBegin, &secEnd,
		); err != nil {
			return nil, fmt.Errorf("scan ticket: %w", err)
		}
		// 库里存的是分，统一换算成元，业务层与模型都按元处理。
		item.Price = float64(payValue) / 100
		item.OriginalPrice = float64(actualVal) / 100
		item.HasSeats = hasSeats
		item.OnSale = true
		item.SoldOut = item.Stock <= 0
		item.SeckillStock = secStock
		item.SeckillBegin = formatNullTime(secBegin)
		item.SeckillEnd = formatNullTime(secEnd)
		item.IsSeckill = secBegin.Valid || secEnd.Valid
		if item.IsSeckill {
			item.Stock = max(0, secStock)
			item.SoldOut = item.Stock <= 0
		}
		item.SeckillActive = item.IsSeckill && item.Stock > 0 && seckillRunning(item.SeckillBegin, item.SeckillEnd)
		item.StockSource = "mysql_snapshot"
		item.DetailURL = ShowURL(item.ShowID)
		item.Rules = truncateRunes(item.Rules, 800)
		items = append(items, item)
	}
	return items, rows.Err()
}

func formatNullTime(value sql.NullTime) string {
	if !value.Valid {
		return ""
	}
	return value.Time.Format("2006-01-02 15:04")
}

// normalizeAPITime 把接口返回的 ISO 时间统一成 2006-01-02 15:04。
func normalizeAPITime(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if parsed, err := time.Parse("2006-01-02T15:04:05", value); err == nil {
		return parsed.Format("2006-01-02 15:04")
	}
	return value
}

func parseInt64(value wireString) int64 {
	id, err := strconv.ParseInt(strings.TrimSpace(string(value)), 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// parseCentsToYuan 处理接口以分返回的金额字符串（例如 avgPrice=12300 表示 123 元）。
func parseCentsToYuan(value wireString) float64 {
	cents, err := strconv.ParseFloat(strings.TrimSpace(string(value)), 64)
	if err != nil {
		return 0
	}
	return cents / 100
}

func truncateRunes(value string, limit int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}
