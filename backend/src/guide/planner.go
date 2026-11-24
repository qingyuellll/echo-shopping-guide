package guide

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LYP-leo/echo-shopping-guide/backend/src/ticketing"
)

// Preferences are returned to the browser so a follow-up survives truncated chat history.
type shoppingPlan struct {
	PartySize      int              `json:"party_size"`
	Budget         float64          `json:"budget"`
	BudgetMode     string           `json:"budget_mode"`
	SeatPreference string           `json:"seat_preference"`
	Filters        discoveryFilters `json:"filters"`
}

var peopleRE = regexp.MustCompile(`([0-9]+|一|二|两|三|四|五|六|七|八|九|十)\s*(?:个?人|位|张票)`)
var amountRE = regexp.MustCompile(`(?:预算|总价|合计|总共|每人|人均|每张)\s*(?:不超过|最多|改为|改成|为|是|到)?\s*[¥￥]?\s*(\d+(?:\.\d+)?)|([0-9]+(?:\.[0-9]+)?)\s*(?:元|块|以内|以下)`)
var dateRE = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)

func resolvePlan(input assistRequest, snapshot ticketing.CatalogSnapshot, now time.Time) shoppingPlan {
	p := shoppingPlan{PartySize: 1, BudgetMode: "total", Filters: input.Filters}
	messages := []string{}
	if input.Plan != nil {
		p = *input.Plan
		// Nonempty controls are explicit constraints; conversational corrections below win.
		f := input.Filters
		if f.Query != "" {
			p.Filters.Query = f.Query
		}
		if f.Category != "" {
			p.Filters.Category = f.Category
		}
		if f.City != "" {
			p.Filters.City = f.City
		}
		if f.DateFrom != "" {
			p.Filters.DateFrom = f.DateFrom
		}
		if f.DateTo != "" {
			p.Filters.DateTo = f.DateTo
		}
		if f.Lifecycle != "" && f.Lifecycle != "all" {
			p.Filters.Lifecycle = f.Lifecycle
		}
		if f.MinScore > 0 {
			p.Filters.MinScore = f.MinScore
		}
		if f.AvailableOnly {
			p.Filters.AvailableOnly = true
		}
	} else {
		for _, m := range input.History {
			if m.Role == "user" {
				messages = append(messages, m.Content)
			}
		}
	}
	if p.PartySize == 0 {
		p.PartySize = 1
	}
	if p.BudgetMode == "" {
		p.BudgetMode = "total"
	}
	if input.Budget > 0 {
		p.Budget = input.Budget
		p.BudgetMode = "per_person"
	} else if input.Plan == nil && input.Filters.Budget > 0 {
		p.Budget = input.Filters.Budget
		p.BudgetMode = "per_person"
	}
	p.Filters.Query = defaultString(p.Filters.Query, input.Query)
	messages = append(messages, input.Message)
	for _, message := range messages {
		if m := peopleRE.FindAllStringSubmatch(message, -1); len(m) > 0 {
			value := m[len(m)-1][1]
			n, _ := strconv.Atoi(value)
			if n == 0 {
				n = map[string]int{"一": 1, "二": 2, "两": 2, "三": 3, "四": 4, "五": 5, "六": 6, "七": 7, "八": 8, "九": 9, "十": 10}[value]
			}
			p.PartySize = n
		}
		if matches := amountRE.FindAllStringSubmatch(message, -1); len(matches) > 0 {
			m := matches[len(matches)-1]
			p.Budget, _ = strconv.ParseFloat(defaultString(m[1], m[2]), 64)
			p.BudgetMode = "total"
			if strings.Contains(message, "人均") || strings.Contains(message, "每人") || strings.Contains(message, "每张") {
				p.BudgetMode = "per_person"
			}
		}
		if strings.Contains(message, "不限预算") || strings.Contains(message, "预算不限") {
			p.Budget = 0
		}
		if m := minimumScore.FindAllStringSubmatch(message, -1); len(m) > 0 {
			p.Filters.MinScore, _ = strconv.ParseFloat(m[len(m)-1][1], 64)
		}
		cities := append(append([]string{}, snapshot.Cities...), "北京", "上海", "广州", "深圳", "杭州", "成都", "南京", "武汉", "天津", "重庆", "西安", "长沙", "苏州", "郑州", "青岛", "厦门", "昆明")
		last := -1
		for _, city := range cities {
			if i := strings.LastIndex(message, city); i > last {
				last = i
				p.Filters.City = city
			}
		}
		last = -1
		for _, category := range snapshot.Categories {
			if i := strings.LastIndex(message, category); i > last {
				last = i
				p.Filters.Category = category
			}
		}
		if strings.Contains(message, "不限城市") {
			p.Filters.City = ""
		}
		if strings.Contains(message, "不限类型") {
			p.Filters.Category = ""
		}
		if strings.Contains(message, "近一点") || strings.Contains(message, "前排") || strings.Contains(message, "坐近") {
			p.SeatPreference = "front"
		}
		if strings.Contains(message, "连座") || strings.Contains(message, "坐一起") || strings.Contains(message, "相邻") {
			p.SeatPreference = "adjacent"
		}
		if strings.Contains(message, "不限座位") || strings.Contains(message, "不挑座位") {
			p.SeatPreference = ""
		}
		if strings.Contains(message, "便宜") || strings.Contains(message, "低价") {
			p.Filters.Sort = "price"
		}
		if strings.Contains(message, "口碑优先") || strings.Contains(message, "评分优先") {
			p.Filters.Sort = "reputation"
		}
		dates := dateRE.FindAllString(message, -1)
		if len(dates) > 0 {
			p.Filters.DateFrom = dates[0]
			p.Filters.DateTo = dates[len(dates)-1]
		} else {
			day := -1
			for i, name := range []string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"} {
				if strings.Contains(message, name) || strings.Contains(message, strings.Replace(name, "周", "星期", 1)) {
					day = i
				}
			}
			if strings.Contains(message, "周末") {
				day = 6
			}
			if day >= 0 {
				zone := time.FixedZone("Asia/Shanghai", 8*3600)
				local := now.In(zone)
				midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone)
				delta := (day - int(local.Weekday()) + 7) % 7
				if strings.Contains(message, "下周") {
					delta = (8-int(local.Weekday()))%7 + ((day + 6) % 7)
					if local.Weekday() == time.Sunday {
						delta = 1 + (day+6)%7
					}
				}
				date := midnight.AddDate(0, 0, delta)
				p.Filters.DateFrom = date.Format("2006-01-02")
				p.Filters.DateTo = p.Filters.DateFrom
				if strings.Contains(message, "周末") {
					p.Filters.DateTo = date.AddDate(0, 0, 1).Format("2006-01-02")
				}
			}
		}
		if strings.Contains(message, "不限日期") || strings.Contains(message, "时间不限") {
			p.Filters.DateFrom = ""
			p.Filters.DateTo = ""
		}
	}
	p.Filters.Sort = defaultString(p.Filters.Sort, "reputation")
	// Popular is the browsing default; conversational recommendations use sample-aware reputation.
	if p.Filters.Sort == "popular" {
		p.Filters.Sort = "reputation"
	}
	p.Filters.Budget = p.Budget
	if p.BudgetMode == "total" && p.PartySize > 0 {
		p.Filters.Budget = p.Budget / float64(p.PartySize)
	}
	return p
}

func plannedTickets(tiers []ticketing.TicketTier, p shoppingPlan) []ticketing.TicketTier {
	result := []ticketing.TicketTier{}
	for _, t := range tiers {
		if !t.OnSale || t.SoldOut || t.Stock < p.PartySize || t.IsSeckill && (!t.SeckillActive || p.PartySize > 1) || p.Filters.Budget > 0 && t.Price > p.Filters.Budget || p.SeatPreference != "" && !t.HasSeats {
			continue
		}
		if u, err := url.Parse(t.CheckoutURL); err == nil {
			q := u.Query()
			q.Set("count", strconv.Itoa(p.PartySize))
			if p.Budget > 0 {
				total := p.Budget
				if p.BudgetMode == "per_person" {
					total *= float64(p.PartySize)
				}
				q.Set("budget", strconv.FormatFloat(total, 'f', 2, 64))
			}
			if p.SeatPreference != "" {
				q.Set("seatPreference", p.SeatPreference)
			}
			u.RawQuery = q.Encode()
			t.CheckoutURL = u.String()
		}
		result = append(result, t)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Price < result[j].Price })
	return result
}

func planDescription(p shoppingPlan) string {
	parts := []string{fmt.Sprintf("%d 人", p.PartySize)}
	if p.Budget > 0 {
		kind := "总预算"
		if p.BudgetMode == "per_person" {
			kind = "每人预算"
		}
		parts = append(parts, fmt.Sprintf("%s ¥%.2f", kind, p.Budget))
	}
	if p.Filters.City != "" {
		parts = append(parts, displayCity(p.Filters.City))
	}
	if p.Filters.Category != "" {
		parts = append(parts, p.Filters.Category)
	}
	if p.Filters.DateFrom != "" {
		parts = append(parts, p.Filters.DateFrom+" 至 "+p.Filters.DateTo)
	}
	if p.SeatPreference != "" {
		parts = append(parts, "座位偏好需在座位图确认")
	}
	return strings.Join(parts, " · ")
}
