#ifndef SSXNG_TRAY_DARWIN_H
#define SSXNG_TRAY_DARWIN_H

/* Implemented in Go (see tray_darwin.go).
   kind: 1 = toggle, 2 = select mode, 3 = show window, 4 = quit, 5 = command copied. */
extern void ssxngTrayEvent(int kind, char *mode);

/* Returns 0 on success. */
int ssxngTrayStart(void);
/* commandsSpec is one "key|title|command" line per copy-command item; "" leaves
   the copy section out of the menu. */
void ssxngTrayRefresh(const char *title, const char *status, const char *currentMode, const char *modesSpec, const char *commandsSpec, int enabled);
void ssxngTrayStop(void);
int ssxngSetDockIcon(int visible);

#endif /* SSXNG_TRAY_DARWIN_H */
