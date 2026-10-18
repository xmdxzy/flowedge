package service

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CronSpec 五段式 cron 表达式（分 时 日 月 周）
// 支持：*、*/n、a-b、a-b/n、a,b,c 的任意组合
type CronSpec struct {
	minute map[int]bool
	hour   map[int]bool
	dom    map[int]bool
	month  map[int]bool
	dow    map[int]bool
	Raw    string
}

// ParseCron 解析 cron 表达式
func ParseCron(expr string) (*CronSpec, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return nil, fmt.Errorf("需要 5 段（分 时 日 月 周），当前 %d 段", len(fields))
	}
	ranges := [][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	sets := make([]map[int]bool, 5)
	for i, f := range fields {
		set, err := parseCronField(f, ranges[i][0], ranges[i][1])
		if err != nil {
			return nil, fmt.Errorf("第 %d 段 %q：%w", i+1, f, err)
		}
		sets[i] = set
	}
	return &CronSpec{
		minute: sets[0], hour: sets[1], dom: sets[2], month: sets[3], dow: sets[4],
		Raw: expr,
	}, nil
}

func parseCronField(field string, lo, hi int) (map[int]bool, error) {
	set := make(map[int]bool)
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		step := 1
		if idx := strings.Index(part, "/"); idx >= 0 {
			s, err := strconv.Atoi(part[idx+1:])
			if err != nil || s <= 0 {
				return nil, fmt.Errorf("步长无效")
			}
			step = s
			part = part[:idx]
		}

		start, end := lo, hi
		switch {
		case part == "*" || part == "":
			// 全范围
		case strings.Contains(part, "-"):
			ab := strings.SplitN(part, "-", 2)
			a, err1 := strconv.Atoi(ab[0])
			b, err2 := strconv.Atoi(ab[1])
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("范围无效")
			}
			start, end = a, b
		default:
			v, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("数值无效")
			}
			start, end = v, v
		}
		if start < lo || end > hi || start > end {
			return nil, fmt.Errorf("超出范围 %d-%d", lo, hi)
		}
		for v := start; v <= end; v += step {
			set[v] = true
		}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("没有有效取值")
	}
	return set, nil
}

// Match 判断某个时刻是否命中
func (c *CronSpec) Match(t time.Time) bool {
	return c.minute[t.Minute()] &&
		c.hour[t.Hour()] &&
		c.dom[t.Day()] &&
		c.month[int(t.Month())] &&
		c.dow[int(t.Weekday())]
}

// Describe 生成人话描述（前端展示）
func (c *CronSpec) Describe() string {
	parts := strings.Fields(c.Raw)
	if len(parts) != 5 {
		return c.Raw
	}
	if parts[0] == "*" && parts[1] == "*" && parts[2] == "*" && parts[3] == "*" && parts[4] == "*" {
		return "每分钟"
	}
	if strings.HasPrefix(parts[0], "*/") && parts[1] == "*" && parts[2] == "*" && parts[3] == "*" && parts[4] == "*" {
		return "每 " + strings.TrimPrefix(parts[0], "*/") + " 分钟"
	}
	if parts[0] != "*" && parts[1] == "*" && parts[2] == "*" && parts[3] == "*" && parts[4] == "*" {
		return "每小时的第 " + parts[0] + " 分钟"
	}
	if parts[0] != "*" && parts[1] != "*" && parts[2] == "*" && parts[3] == "*" && parts[4] == "*" {
		return fmt.Sprintf("每天 %02s:%02s", parts[1], parts[0])
	}
	return c.Raw
}
