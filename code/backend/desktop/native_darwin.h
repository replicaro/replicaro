#ifndef REPLICARO_NATIVE_DARWIN_H
#define REPLICARO_NATIVE_DARWIN_H

void ReplicaroRunApp(void);
void ReplicaroStopApp(void);
void ReplicaroReplyToTerminate(void);
int ReplicaroIsMainThread(void);
int ReplicaroOpenURL(const char *target);
int ReplicaroShowNotification(const char *title, const char *message, const char *target, const char *iconPath);
int ReplicaroNotificationCapability(void);
void ReplicaroShowStartupFailure(const char *message);
int ReplicaroConfirmStartupRepair(const char *message, const char *repairTitle, const char *declineTitle);
int ReplicaroConfirmQuit(const char *message, const char *quitTitle, const char *cancelTitle);
void ReplicaroSetStatusState(int state, const void *png, int length);

#endif
