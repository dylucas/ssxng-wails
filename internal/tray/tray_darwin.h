#ifndef SSXNG_TRAY_DARWIN_H
#define SSXNG_TRAY_DARWIN_H

/* Implemented in Go (see tray_darwin.go).
   kind: 1 = toggle, 2 = select mode, 3 = show window, 4 = quit. */
extern void ssxngTrayEvent(int kind, char *mode);

/* Returns 0 on success. */
int ssxngTrayStart(void);
void ssxngTrayRefresh(const char *title, const char *status, const char *currentMode, const char *modesSpec, int enabled);
void ssxngTrayStop(void);
int ssxngSetDockIcon(int visible);

#endif /* SSXNG_TRAY_DARWIN_H */
