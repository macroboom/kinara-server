package transcode

import "time"

// Rung is one step of the quality ladder players step down.
type Rung struct {
	Bitrate int `json:"maxBitrate"` // kbit/s
	Height  int `json:"maxHeight"`
}

// Ladder is the qualities a player steps down through, best first. Each is
// a conversion to H.264 at that bitrate and at most that height.
var Ladder = []Rung{
	{40000, 2160},
	{20000, 2160},
	{12000, 1080},
	{8000, 1080},
	{5000, 1080},
	{3000, 720},
	{1500, 480},
	{800, 360},
}

// NextRung is the step below a conversion at bitrate and height (0 for the
// original picture: the first rung below what the file has). ok is false
// at the bottom.
func NextRung(bitrate, height int) (Rung, bool) {
	for _, r := range Ladder {
		if bitrate > 0 && r.Bitrate > bitrate*4/5 {
			continue
		}
		if height > 0 && r.Height > height {
			continue
		}
		if bitrate == 0 && height == 0 {
			// From the original: a solid 1080p first.
			if r.Bitrate > 12000 {
				continue
			}
		}
		return r, true
	}
	return Rung{}, false
}

// Advice tells a player whether to step its quality down.
type Advice struct {
	// Action is "ok" (carry on) or "lower" (start again at Next).
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
	Next   *Rung  `json:"next,omitempty"`
	// Stream is the player's conversion, when one is running.
	Stream *LiveInfo `json:"stream,omitempty"`
}

// Advise looks at the conversions running and says what a device playing a
// version should do:
//   - its own conversion can't keep up: step down, unless it's the
//     priority device and others can step down for it instead;
//   - the priority device can't keep up: everyone else steps down first.
//
// Only re-encoded streams can step down; a stream that keeps the original
// picture costs the server little.
func (t *Transcoder) Advise(device, versionID string) Advice {
	now := time.Now()
	var mine *Live
	var mineInfo LiveInfo
	var infos []LiveInfo
	ls := t.lives()
	for _, l := range ls {
		in := l.info(now)
		infos = append(infos, in)
		if in.Device == device && in.VersionID == versionID {
			// The newest one is the one playing.
			mine, mineInfo = l, in
		}
	}
	if mine == nil {
		return Advice{Action: "ok"}
	}
	adv := Advice{Action: "ok", Stream: &mineInfo}
	canLower := func(in LiveInfo) bool {
		_, ok := NextRung(in.MaxBitrate, in.MaxHeight)
		return in.Video == "h264" && ok
	}
	lower := func(reason string) Advice {
		next, _ := NextRung(mineInfo.MaxBitrate, mineInfo.MaxHeight)
		mine.mu.Lock()
		mine.advised = true
		mine.mu.Unlock()
		adv.Action, adv.Reason, adv.Next = "lower", reason, &next
		return adv
	}
	// Others who could give way for a priority stream: re-encoding, settled
	// in (a fresh stream may already be a step down), not priority.
	var others []LiveInfo
	priorityBehind := false
	for _, in := range infos {
		if in.ID == mineInfo.ID {
			continue
		}
		if in.Priority && in.Behind {
			priorityBehind = true
		}
		if !in.Priority && canLower(in) && now.Sub(in.StartedAt) >= warmup {
			others = append(others, in)
		}
	}

	switch {
	case mineInfo.Behind && mineInfo.Priority && len(others) > 0:
		// The others are asked on their next check; this one holds.
		adv.Reason = "other devices are stepping down so this one keeps its quality"
	case mineInfo.Behind && canLower(mineInfo):
		return lower("the server can't convert this fast enough")
	case mineInfo.Behind && mineInfo.Video != "h264":
		// Keeping the original picture and still behind: the disk or the
		// network, not the converting. Re-encoding smaller may help.
		if next, ok := NextRung(0, 0); ok {
			mine.mu.Lock()
			mine.advised = true
			mine.mu.Unlock()
			adv.Action, adv.Reason, adv.Next = "lower", "the server can't send this fast enough", &next
		}
	case priorityBehind && !mineInfo.Priority && canLower(mineInfo) && now.Sub(mineInfo.StartedAt) >= warmup:
		return lower("making room for the priority device")
	}
	return adv
}
