package bandwidth

import (
	"errors"
	"time"
)

// ScheduledModel enveloppe un SpeedModel et multiplie sa vitesse par un
// coefficient horaire. Le coefficient hourly[h] est atteint à h:00
// (heure locale de loc) ; entre deux heures il est interpolé linéairement, et
// après 23:00 l'interpolation rejoint hourly[0] (passage de minuit). L'heure
// vient uniquement de l'argument de Next (l'horloge injectée du Dispatcher).
//
// Changement d'heure : le coefficient dépend de l'heure murale. À la
// transition de printemps (02:00 -> 03:00) la courbe saute de l'heure 1 à
// l'heure 3 sans passer par la valeur de 02:00 ; à l'automne l'heure 02:xx est
// vécue deux fois, avec la même courbe. Aucune discontinuité d'`uploaded` n'en
// résulte : seule la vitesse change, jamais les compteurs.
type ScheduledModel struct {
	inner  SpeedModel
	hourly [24]float64
	loc    *time.Location
}

// NewScheduledModel construit l'enveloppe. Les coefficients sont bornés à [0, 2].
func NewScheduledModel(inner SpeedModel, hourly [24]float64, loc *time.Location) (*ScheduledModel, error) {
	if inner == nil {
		return nil, errors.New("bandwidth: modèle de vitesse manquant")
	}
	if loc == nil {
		loc = time.Local
	}
	for i, v := range hourly {
		hourly[i] = min(max(v, 0), 2)
	}
	return &ScheduledModel{inner: inner, hourly: hourly, loc: loc}, nil
}

// Multiplier renvoie le coefficient à l'instant now.
func (m *ScheduledModel) Multiplier(now time.Time) float64 {
	t := now.In(m.loc)
	h := t.Hour()
	frac := (float64(t.Minute()*60+t.Second()) + float64(t.Nanosecond())/1e9) / 3600
	a, b := m.hourly[h], m.hourly[(h+1)%24]
	return a + (b-a)*frac
}

// Next implémente SpeedModel.
func (m *ScheduledModel) Next(now time.Time) int64 {
	return int64(float64(m.inner.Next(now)) * m.Multiplier(now))
}
