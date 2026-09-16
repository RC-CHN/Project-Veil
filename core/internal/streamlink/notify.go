package streamlink

// Changes returns a channel closed by the next state change. Capture it before
// checking Status to avoid a lost wakeup when combining several links in select.
// The caller must obtain a fresh channel after each wakeup and must not close it.
func (l *Link) Changes() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.changed
}
