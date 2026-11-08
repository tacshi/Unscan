package pdfocr

import (
	"strings"
	"unicode"

	"unscan/internal/ocr"
)

// Traditional Chinese, as Taiwan and Hong Kong print it, is set in faces
// of its own forms, which differ from simplified Chinese's in more than
// its characters: its punctuation is centered in the em, and characters
// both share differ in strokes. Which a text is in is told by characters
// of either's alone, among the commonest that differ: simplified is the
// first of each pair, traditional the second.
const simplified, traditional = "们个这来说时为会学国与对开关长点现经发进动实种样机问间体还当从应总给电话书习语见车门东号马头线认识读写难欢岁钱买卖贵饭馆楼钟鸟鱼龙过没让请谢边题员单级场报业务义议论设计资产变历区运连远选铁银错陈张杨刘赵孙郑罗华汉爱气无万亲农兴齐厅厂广卫飞图团园圆处备够宝层岛师帮带归录怀态战护担拥择换数断显杂条极构标树检毕决况净测济满灯热状独环疗尽监码础离积称稳笔简类红约纪纸练组细终结统继续维网联职胜脑脸节艺药营虽补装观规视览觉订训记讲许证评词试诗该详诉误课调谈质购赛赶软轮转轻较辆输办达迁递遗邮针钢镇闭闻阅队阳阴际陆随险隐须顾预领频颜风饮驾验鲜吗绍乐饰戏欧闹齿龟鸡猫杀举临丽严丢侧债伞伤价优众爷硕",
	"們個這來說時為會學國與對開關長點現經發進動實種樣機問間體還當從應總給電話書習語見車門東號馬頭線認識讀寫難歡歲錢買賣貴飯館樓鐘鳥魚龍過沒讓請謝邊題員單級場報業務義議論設計資產變歷區運連遠選鐵銀錯陳張楊劉趙孫鄭羅華漢愛氣無萬親農興齊廳廠廣衛飛圖團園圓處備夠寶層島師幫帶歸錄懷態戰護擔擁擇換數斷顯雜條極構標樹檢畢決況淨測濟滿燈熱狀獨環療盡監碼礎離積稱穩筆簡類紅約紀紙練組細終結統繼續維網聯職勝腦臉節藝藥營雖補裝觀規視覽覺訂訓記講許證評詞試詩該詳訴誤課調談質購賽趕軟輪轉輕較輛輸辦達遷遞遺郵針鋼鎮閉聞閱隊陽陰際陸隨險隱須顧預領頻顏風飲駕驗鮮嗎紹樂飾戲歐鬧齒龜雞貓殺舉臨麗嚴丟側債傘傷價優眾爺碩"

// scripts counts the characters of texts that are traditional Chinese's
// alone, t, and simplified Chinese's alone, s.
func scripts(texts ...string) (t, s int) {
	for _, text := range texts {
		for _, r := range text {
			switch {
			case strings.ContainsRune(traditional, r):
				t++
			case strings.ContainsRune(simplified, r):
				s++
			}
		}
	}
	return t, s
}

// isTraditional reports whether texts are in traditional Chinese: more of
// their characters are traditional Chinese's alone than simplified's, two
// at least.
func isTraditional(texts []string) bool {
	t, s := scripts(texts...)
	return t >= 2 && t > s
}

// isSimplified reports whether texts are in simplified Chinese, as
// isTraditional tells traditional Chinese.
func isSimplified(texts []string) bool {
	t, s := scripts(texts...)
	return s >= 2 && s > t
}

// variants maps characters the recognizer reads in the forms of other
// scripts, Japanese or simplified Chinese, where a stroke or two tells
// them apart, to traditional Chinese's: 説, 内, 黄 and 髪 for 說, 內, 黃
// and 髮.
var variants = strings.NewReplacer(
	"説", "說", "悦", "悅", "税", "稅", "鋭", "銳", "脱", "脫", "閲", "閱", "兑", "兌", "歳", "歲",
	"黒", "黑", "戸", "戶", "呉", "吳", "縁", "緣", "緑", "綠", "録", "錄", "涙", "淚", "黄", "黃",
	"曽", "曾", "戻", "戾", "内", "內", "却", "卻", "吕", "呂", "衆", "眾", "髪", "髮",
)

// toSimplified maps traditional Chinese's characters to simplified
// Chinese's, for those the recognizer reads in a line of simplified
// Chinese in traditional forms: 漢語 for 汉语.
var toSimplified = func() *strings.Replacer {
	t, s := []rune(traditional), []rune(simplified)
	pairs := make([]string, 0, 2*len(t))
	for i := range t {
		pairs = append(pairs, string(t[i]), string(s[i]))
	}
	return strings.NewReplacer(pairs...)
}()

// chineseForms sets the lines read from a page in the forms of the
// Chinese it is in: a page in traditional Chinese in its forms (see
// variants), and a page in simplified Chinese in its, but for a line that
// reads as traditional Chinese by itself, as a quotation in it does, or a
// line of Japanese, whose kanji share traditional Chinese's forms. Other
// pages, as of Japanese, it leaves as they are.
func chineseForms(lines []ocr.Line) []ocr.Line {
	texts := make([]string, len(lines))
	for i, l := range lines {
		texts[i] = l.Text
	}
	switch {
	case isTraditional(texts):
		for i := range lines {
			lines[i].Text = variants.Replace(lines[i].Text)
		}
	case isSimplified(texts):
		for i := range lines {
			if !isTraditional([]string{lines[i].Text}) && !hasKana(lines[i].Text) {
				lines[i].Text = toSimplified.Replace(lines[i].Text)
			}
		}
	}
	return lines
}

// hasKana reports whether text has hiragana or katakana: Japanese.
func hasKana(text string) bool {
	for _, r := range text {
		if unicode.In(r, unicode.Hiragana, unicode.Katakana) {
			return true
		}
	}
	return false
}
