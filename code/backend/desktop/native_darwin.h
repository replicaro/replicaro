#ifndef REPLICARO_NATIVE_DARWIN_H
#define REPLICARO_NATIVE_DARWIN_H

void ReplicaroRunApp(void);
void ReplicaroStopApp(void);
int ReplicaroIsMainThread(void);
int ReplicaroOpenURL(const char *target);
int ReplicaroShowNotification(const char *title, const char *message, const char *target, const char *iconPath);
int ReplicaroNotificationCapability(void);

#endif
