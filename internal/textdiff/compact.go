package textdiff

// compact normalizes an edit script the way git does before printing one.
//
// The shortest edit script is not unique. When a changed line sits next to an
// identical one, the change can be written in several places for the same
// cost, and a raw search picks whichever the walk happened to reach first.
// The result is a diff that is correct and reads badly: a function added at
// the end of a file can come out as a change to its first line and an
// addition of the rest.
//
// Git resolves this by sliding each run of changed lines as far down the file
// as it can go without changing what the patch means. The same rule is
// applied here, so a reviewer — and the model reading the patch — sees the
// change where it actually happened.
//
// Rebuilding the script from the two sides also puts every removal of a run
// before its additions, which is the order every diff tool prints and the
// order a reader expects.
func compact(script []step, old, cur []string) []step {
	changedOld := make([]bool, len(old))
	changedCur := make([]bool, len(cur))
	for _, s := range script {
		switch s.kind {
		case opDelete:
			changedOld[s.old] = true
		case opInsert:
			changedCur[s.cur] = true
		case opEqual:
		}
	}

	slide(changedOld, old)
	slide(changedCur, cur)

	return rebuild(changedOld, changedCur)
}

// slide moves each run of changed lines as far towards the end of the file as
// it can go while still describing the same change.
//
// A run may move down by one when the line just past it is unchanged and
// equal to the run's first line: dropping the first and taking that one
// removes and adds exactly the same set of lines.
func slide(changed []bool, lines []string) {
	for i := 0; i < len(changed); i++ {
		if !changed[i] {
			continue
		}
		start := i
		end := i
		for end < len(changed) && changed[end] {
			end++
		}
		for end < len(changed) && !changed[end] && lines[start] == lines[end] {
			changed[start] = false
			changed[end] = true
			start++
			end++
		}
		i = end - 1
	}
}

// rebuild turns the two sides back into a script.
func rebuild(changedOld, changedCur []bool) []step {
	out := make([]step, 0, len(changedOld)+len(changedCur))
	i, j := 0, 0
	for i < len(changedOld) || j < len(changedCur) {
		switch {
		case i < len(changedOld) && changedOld[i]:
			// Every removal of this run comes first, then its additions.
			for i < len(changedOld) && changedOld[i] {
				out = append(out, step{kind: opDelete, old: i, cur: j})
				i++
			}
			for j < len(changedCur) && changedCur[j] {
				out = append(out, step{kind: opInsert, old: i, cur: j})
				j++
			}
		case j < len(changedCur) && changedCur[j]:
			for j < len(changedCur) && changedCur[j] {
				out = append(out, step{kind: opInsert, old: i, cur: j})
				j++
			}
		default:
			out = append(out, step{kind: opEqual, old: i, cur: j})
			i++
			j++
		}
	}
	return out
}
