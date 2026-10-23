package model

// WrapReminder wraps text in a <system-reminder> block, the convention for
// context that the application, not the user, adds to a message.
func WrapReminder(text string) string {
	return "<system-reminder>\n" + text + "\n</system-reminder>"
}
