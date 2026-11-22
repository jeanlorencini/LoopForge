package httpapi

import (
	"regexp"
	"time"
)

// assistantClock is the user-facing time zone. The platform stores and returns
// UTC, but people read replies in Brasília time; a model quoting a raw UTC
// timestamp tells them a run started at 17:16 when it was 14:16.
var assistantClock = func() *time.Location {
	if loc, err := time.LoadLocation("America/Sao_Paulo"); err == nil {
		return loc
	}
	// Brazil has had no daylight saving time since 2019.
	return time.FixedZone("America/Sao_Paulo", -3*3600)
}()

var assistantNow = time.Now

var utcTimestampPattern = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z\b`)

// beijingTimestamps rewrites RFC 3339 UTC timestamps in a platform tool result
// to the same instant in assistantClock (-03:00); the upstream name is kept. Fractional seconds are kept, so a value the
// model passes back (for example an expectedUpdatedAt guard) still compares
// equal on the server.
func beijingTimestamps(text string) string {
	return utcTimestampPattern.ReplaceAllStringFunc(text, func(raw string) string {
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return raw
		}
		return t.In(assistantClock).Format(time.RFC3339Nano)
	})
}

// assistantPlatformResultForModel prepares a platform tool result for the
// model. Artifact bodies are documents the model may quote or edit verbatim,
// so their text is left untouched. Oversized list results keep their leading
// items with a notice instead of overflowing into a file the model cannot
// parse.
func assistantPlatformResultForModel(tool, text string) string {
	if tool == "runs_artifact_get" {
		return text
	}
	return fitAssistantToolResult(tool, beijingTimestamps(text))
}

var assistantWeekdays = [...]string{"domingo", "segunda-feira", "terça-feira", "quarta-feira", "quinta-feira", "sexta-feira", "sábado"}

// assistantClockNote tells the model the current Brasília time and how to state
// times, so "today", "just now" and run timings match the user's clock.
func assistantClockNote() string {
	now := assistantNow().In(assistantClock)
	return "\nHorário atual de Brasília: " + now.Format("2006-01-02 15:04") + " (" + assistantWeekdays[now.Weekday()] + "). Nas respostas, escreva todos os horários no horário de Brasília (UTC-3) e não escreva UTC. Os horários nos resultados das ferramentas da plataforma já estão em -03:00: leia como estão. Tarefas agendadas com cron também usam o horário de Brasília."
}
