package fluxcore

// Weather is the "network forecast" from spec §7: a calm, human status now and
// projected a little into the future. It is explicitly a *projection* of the
// current trend, not a prediction of external events — honesty over theatre.

// Condition is a coarse, user-facing band.
type Condition string

const (
	Excellent Condition = "excellent"
	Good      Condition = "good"
	Fair      Condition = "fair"
	Degrading Condition = "degrading"
	Unstable  Condition = "unstable"
)

// bandFor maps a 0..100 score to a condition.
func bandFor(score float64) Condition {
	switch {
	case score >= 85:
		return Excellent
	case score >= 70:
		return Good
	case score >= 50:
		return Fair
	case score >= 30:
		return Degrading
	default:
		return Unstable
	}
}

// Forecast is the now / +15 / +30 / +60 outlook (in minutes) for the system.
type Forecast struct {
	Now    Condition `json:"now"`
	Plus15 Condition `json:"plus15"`
	Plus30 Condition `json:"plus30"`
	Plus60 Condition `json:"plus60"`
}

// WeatherFrom projects a forecast from the current best score and its recent
// history. The slope of recent scores is extrapolated forward; with too little
// history we simply hold the current condition rather than invent a trend.
func WeatherFrom(currentScore float64, scoreHistory []float64) Forecast {
	now := bandFor(currentScore)
	if len(scoreHistory) < 4 {
		return Forecast{now, now, now, now}
	}
	slope := linregSlope(scoreHistory) // per sample; treat a sample as ~1 min of trend

	proj := func(minutes float64) Condition {
		s := currentScore + slope*minutes
		if s > 100 {
			s = 100
		}
		if s < 0 {
			s = 0
		}
		return bandFor(s)
	}
	return Forecast{
		Now:    now,
		Plus15: proj(15),
		Plus30: proj(30),
		Plus60: proj(60),
	}
}
