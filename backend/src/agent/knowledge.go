package agent

import (
	"sort"
	"strings"
)

// knowledgeBase 是内置的「观演常识」检索库。
//
// 它只放通用规则（入场、儿童、退改、选座、交通、礼仪），不放任何具体演出的
// 场次、票价、余票——那些必须来自工具查询。回答时会明确标注「通用建议，
// 具体以场馆公告为准」，避免模型把常识包装成事实。
var knowledgeBase = []KnowledgeHit{
	{Title: "入场时间与迟到", Aliases: []string{"迟到", "提前多久", "几点入场", "进场"}, Content: "多数剧场提前 30–60 分钟开放入场；迟到观众通常需等幕间或工作人员指定时点才能进入，且可能被安排到就近空位，不保证原座位。"},
	{Title: "退改规则怎么读", Aliases: []string{"能退吗", "退票", "退款", "改签", "不可退"}, Content: "票档的退改规则逐档不同：有的演出前 3 天可退，有的标注不可退。下单前请在确认页看清楚，特价票与秒杀票通常不可退换。"},
	{Title: "儿童与身高限制", Aliases: []string{"带孩子", "小孩", "儿童", "身高", "几岁"}, Content: "儿童剧通常一人一票，部分场次对身高有要求；非儿童类演出可能不建议学龄前儿童入场。以场馆与主办方公告为准。"},
	{Title: "选座：前排还是看台", Aliases: []string{"坐哪", "前排", "看台", "视野", "位置"}, Content: "话剧、音乐剧更看重正面视角，一楼中区优先；大型演唱会前排偏音墙，稍靠后可听清整体声场。需要连座时优先选同一票档里的相邻区域。"},
	{Title: "连座怎么提高成功率", Aliases: []string{"坐一起", "连号", "相邻", "挨着"}, Content: "连座取决于同一票档的余票分布。人数多时先看余票是否大于人数，再在座位图确认相邻；秒杀票通常不支持多人连座。"},
	{Title: "交通与到场缓冲", Aliases: []string{"停车", "地铁", "怎么去", "交通", "堵车"}, Content: "尽量预留 30 分钟缓冲：取票、安检、找座位都可能排队。自驾需提前确认场馆停车位，散场时段打车排队较长。"},
	{Title: "观演礼仪", Aliases: []string{"拍照", "录像", "手机", "静音"}, Content: "演出中请勿拍照录像（部分主办方允许谢幕时拍摄），手机调静音，避免在演出中进出。"},
	{Title: "字幕与语言", Aliases: []string{"字幕", "外语", "听不懂", "翻译"}, Content: "外语剧目常配中文字幕，位置偏后或偏侧可能看不清；如对字幕依赖强，选座时优先中区。"},
	{Title: "无障碍与协助", Aliases: []string{"轮椅", "无障碍", "行动不便", "陪同"}, Content: "需要轮椅位、无障碍通道或陪同协助时，建议提前联系场馆服务电话确认，部分场馆的无障碍座位不通过线上选座开放。"},
	{Title: "秒杀票的注意点", Aliases: []string{"秒杀", "抢票", "限购", "特价票"}, Content: "秒杀票通常限单人单张、不可退换，且需要在上架时间窗内下单；显示「抢票中」才代表活动正在进行。"},
	{Title: "散场与返程", Aliases: []string{"散场", "末班车", "返程", "打车", "回去"}, Content: "热门演出散场集中，地铁末班车前后容易拥堵；跨城观演建议预留返程时间或就近住宿。"},
	{Title: "天气与着装", Aliases: []string{"冷吗", "外套", "穿什么", "空调", "户外"}, Content: "户外场次注意天气与温差，室内剧场空调偏凉可带薄外套；着装无硬性要求，舒适即可。"},
}

// SearchKnowledge 是纯词面检索的基线实现：只看标题与正文，不看别名。
//
// 它有两个用途：向量检索不可用时的降级路径，以及检索评估里的对照组。
// 别名（Aliases）故意不参与这里的匹配——否则「接上别名前后差多少」
// 就测不出来了。
func SearchKnowledge(topic string, limit int) []KnowledgeHit {
	return searchKnowledgeKeyword(topic, limit)
}

func searchKnowledgeKeyword(topic string, limit int) []KnowledgeHit {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return nil
	}
	if limit <= 0 || limit > 5 {
		limit = 3
	}
	grams := bigrams(topic)
	scored := make([]KnowledgeHit, 0, len(knowledgeBase))
	for _, entry := range knowledgeBase {
		score := 0
		for _, gram := range grams {
			if strings.Contains(entry.Title, gram) {
				score += 3
			} else if strings.Contains(entry.Content, gram) {
				score++
			}
		}
		if score > 0 {
			hit := entry
			hit.Score = score
			scored = append(scored, hit)
		}
	}
	if len(scored) == 0 {
		return nil
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored
}

// bigrams 把中文串切成二元组，用来做轻量相关度匹配：
// 「儿童身高」→ 儿童 / 童身 / 身高，比按空格分词更适合中文。
func bigrams(text string) []string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) < 2 {
		if len(runes) == 1 {
			return []string{string(runes)}
		}
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(runes)-1)
	for i := 0; i+1 < len(runes); i++ {
		gram := string(runes[i : i+2])
		if seen[gram] || strings.TrimSpace(gram) == "" {
			continue
		}
		seen[gram] = true
		out = append(out, gram)
	}
	return out
}
