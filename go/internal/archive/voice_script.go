package archive

// Мост между сценарием реплея и циклом контроля голоса.
//
// Готовые LoadVoiceThread / LoadVoiceNoteThread здесь не годятся, и разница не
// косметическая: они читают тред ИЗ БАЗЫ целиком, то есть показали бы модели
// реплики, написанные ПОЗЖЕ того момента, в который она должна писать. Это
// классическая утечка будущего в реплее — результат от неё становится лучше, чем
// он есть, и тем сильнее, чем разговорчивее тред. Здесь контекст режется
// моментом по построению: видно ровно то, что видел человек.
//
// Есть и приобретение. У сценария рёбра НАСТОЯЩИЕ, а ники — те, которыми людей
// звали тогда; загрузчик из базы берёт ветку по parent_id (двухуровневой) и
// подписывает её нынешними именами. То есть контекст выходит вернее того, что
// получает `personas voice`.

import "sort"

// ScriptVoiceThread собирает контекст ответа по сценарию, каким он был перед
// репликой с номером upto (сама она и всё после неё не видны).
//
// replyTo — реплика, которой отвечают; 0 означает ответ самой заметке. limit —
// потолок строк ветки: цепочка предков входит в него целиком, остаток добирается
// последними по времени. Предки важнее соседей — без них ответ повисает в
// воздухе, а сосед лишь показывает, о чём вокруг говорят.
func ScriptVoiceThread(sc *ThreadScript, upto int, replyTo int64, selfIDs []int64, limit int) *VoiceThread {
	if upto > len(sc.Comments) {
		upto = len(sc.Comments)
	}
	if upto < 0 {
		upto = 0
	}
	visible := sc.Comments[:upto]

	th := &VoiceThread{
		NoteID:     sc.NoteID,
		NoteText:   excerpt(sc.Note.Text, 1500),
		NoteAuthor: "аноним",
		ReplyToID:  replyTo,
	}
	if sc.Note.AuthorNick != "" {
		th.NoteAuthor = sc.Note.AuthorNick
	}

	byID := make(map[int64]ScriptComment, len(visible))
	for _, c := range visible {
		byID[c.ID] = c
	}
	if t, ok := byID[replyTo]; ok {
		th.AddresseeID, th.AddresseeNick = t.AuthorID, t.AuthorNick
	}
	th.RootID = branchRoot(byID, replyTo)

	self := map[int64]bool{}
	for _, id := range selfIDs {
		self[id] = true
	}

	picked := map[int64]bool{}
	for _, c := range ancestors(byID, replyTo, limit) {
		picked[c.ID] = true
	}
	// Остаток — последние по времени: о чём говорят вокруг прямо сейчас.
	for i := len(visible) - 1; i >= 0 && len(picked) < limit; i-- {
		picked[visible[i].ID] = true
	}

	for _, c := range visible {
		if !picked[c.ID] {
			continue
		}
		m := VoiceThreadMsg{
			ID: c.ID, AuthorID: c.AuthorID, Author: c.AuthorNick,
			Text: excerpt(c.Text, 400), At: c.PublishedAt.Format("2006-01-02"),
			Target: c.ID == replyTo, Self: self[c.AuthorID],
		}
		if m.Self {
			th.SelfInBranch = true
		}
		th.Branch = append(th.Branch, m)
	}
	sort.SliceStable(th.Branch, func(i, j int) bool { return th.Branch[i].ID < th.Branch[j].ID })
	return th
}

// branchRoot — вершина ветки: поднимаемся по настоящим рёбрам, пока есть куда.
func branchRoot(byID map[int64]ScriptComment, id int64) int64 {
	seen := map[int64]bool{}
	for {
		c, ok := byID[id]
		if !ok || c.ReplyTo == 0 || seen[id] {
			return id
		}
		seen[id] = true
		id = c.ReplyTo
	}
}

// ancestors — цепочка от адресата вверх к корню ветки, не длиннее limit.
func ancestors(byID map[int64]ScriptComment, id int64, limit int) []ScriptComment {
	var out []ScriptComment
	seen := map[int64]bool{}
	for len(out) < limit {
		c, ok := byID[id]
		if !ok || seen[id] {
			break
		}
		seen[id] = true
		out = append(out, c)
		id = c.ReplyTo
	}
	return out
}
