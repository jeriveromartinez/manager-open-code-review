package review

// Pass es una pasada de revisión: archivos, reglas que comparten y líneas de diff.
type Pass struct {
	Files     []string
	Rules     []RuleGroup
	DiffLines int
}

// PlanInput son los archivos revisables, sus reglas y los presupuestos de la pasada.
type PlanInput struct {
	Files        []PreviewFile
	Groups       []RuleGroup
	MaxFiles     int
	MaxDiffLines int
	MaxPasses    int
}

// PlanPasses reparte archivos en pasadas. Cada archivo aparece como máximo en
// una pasada; los que no caben en los presupuestos se devuelven sin cubrir.
func PlanPasses(in PlanInput) ([]Pass, []PreviewFile, error) {
	byPath := make(map[string]PreviewFile, len(in.Files))
	for _, f := range in.Files {
		byPath[f.Path] = f
	}

	type bucket struct {
		rules []RuleGroup
		paths []string
	}
	var buckets []bucket
	byRules := make(map[string]int)
	claimed := make(map[string]bool)

	for _, g := range in.Groups {
		idx, ok := byRules[g.Rules]
		if !ok {
			idx = len(buckets)
			byRules[g.Rules] = idx
			buckets = append(buckets, bucket{})
		}
		buckets[idx].rules = append(buckets[idx].rules, g)
		for _, p := range g.Files {
			if _, known := byPath[p]; !known || claimed[p] {
				continue
			}
			claimed[p] = true
			buckets[idx].paths = append(buckets[idx].paths, p)
		}
	}

	var passes []Pass
	flush := func(cur *Pass, rules []RuleGroup) {
		if len(cur.Files) == 0 {
			return
		}
		passes = append(passes, *cur)
		*cur = Pass{Rules: rules}
	}

	stop := false
	for _, b := range buckets {
		if stop || len(b.paths) == 0 {
			continue
		}
		cur := Pass{Rules: b.rules}
		for _, p := range b.paths {
			if len(passes) >= in.MaxPasses {
				stop = true
				break
			}
			f := byPath[p]
			lines := f.Insertions + f.Deletions
			if in.MaxFiles <= 0 || in.MaxDiffLines <= 0 || lines > in.MaxDiffLines {
				continue
			}
			if len(cur.Files) > 0 && (len(cur.Files) >= in.MaxFiles || cur.DiffLines+lines > in.MaxDiffLines) {
				flush(&cur, b.rules)
			}
			cur.Files = append(cur.Files, p)
			cur.DiffLines += lines
		}
		if len(passes) < in.MaxPasses {
			flush(&cur, b.rules)
		}
	}

	covered := make(map[string]bool)
	for _, pass := range passes {
		for _, p := range pass.Files {
			covered[p] = true
		}
	}
	var uncovered []PreviewFile
	for _, f := range in.Files {
		if !covered[f.Path] {
			uncovered = append(uncovered, f)
		}
	}
	return passes, uncovered, nil
}
