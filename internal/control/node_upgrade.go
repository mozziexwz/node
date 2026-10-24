package control

import "errors"

// NodeUpgradeSettings is shown above the deploy page's upgrade form: when the
// node installer last changed and what changed, so members can decide whether
// to upgrade. Both fields are public; empty notes hide the card.
type NodeUpgradeSettings struct {
	ReleasedAt string `json:"releasedAt"`
	Notes      string `json:"notes"`
	UpdatedAt  int64  `json:"updatedAt"`
}

func normalizeNodeUpgrade(in NodeUpgradeSettings) (NodeUpgradeSettings, error) {
	out := NodeUpgradeSettings{UpdatedAt: in.UpdatedAt}
	var err error
	if out.ReleasedAt, err = releaseDate(in.ReleasedAt); err != nil {
		return out, err
	}
	if out.Notes, err = releaseNotes(in.Notes); err != nil {
		return out, err
	}
	if out.Notes != "" && out.ReleasedAt == "" {
		return out, errors.New("填写了更新内容时，请同时填写更新日期")
	}
	return out, nil
}

func loadNodeUpgradeSettings(s *State) NodeUpgradeSettings {
	return loadSiteSetting(s, "nodeUpgrade", NodeUpgradeSettings{}, normalizeNodeUpgrade)
}
