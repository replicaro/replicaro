//go:build darwin && cgo

#import <AppKit/AppKit.h>
#import <UserNotifications/UserNotifications.h>
#import <pthread.h>
#import <stdatomic.h>
#import <stdlib.h>
#import "native_darwin.h"

extern void replicaroOpenRequested(void);
extern void replicaroQuitRequested(void);

static atomic_int replicaroNotificationStatus = UNAuthorizationStatusNotDetermined;

@interface ReplicaroAppDelegate : NSObject <NSApplicationDelegate, UNUserNotificationCenterDelegate>
@property(nonatomic, strong) NSStatusItem *statusItem;
@end

@implementation ReplicaroAppDelegate

- (void)applicationDidFinishLaunching:(NSNotification *)notification {
    (void)notification;
    NSStatusItem *item = [[NSStatusBar systemStatusBar] statusItemWithLength:NSSquareStatusItemLength];
    self.statusItem = item;

    NSImage *image = nil;
    NSString *path = [[NSBundle mainBundle] pathForResource:@"replicaro-favicon" ofType:@"png"];
    if (path != nil) {
        image = [[NSImage alloc] initWithContentsOfFile:path];
        image.size = NSMakeSize(18.0, 18.0);
    }
    if (image != nil) {
        item.button.image = image;
        item.button.imagePosition = NSImageOnly;
    } else {
        item.button.title = @"R";
    }
    item.button.toolTip = @"Replicaro";

    NSMenu *menu = [[NSMenu alloc] initWithTitle:@"Replicaro"];
    NSMenuItem *open = [[NSMenuItem alloc] initWithTitle:@"Open Replicaro"
                                                   action:@selector(openReplicaro:)
                                            keyEquivalent:@""];
    open.target = self;
    [menu addItem:open];
    [menu addItem:[NSMenuItem separatorItem]];
    NSMenuItem *quit = [[NSMenuItem alloc] initWithTitle:@"Quit Replicaro"
                                                   action:@selector(quitReplicaro:)
                                            keyEquivalent:@"q"];
    quit.target = self;
    [menu addItem:quit];
    item.menu = menu;

    if (getenv("REPLICARO_DESKTOP_TEST") == NULL) {
        UNUserNotificationCenter *center = [UNUserNotificationCenter currentNotificationCenter];
        center.delegate = self;
        [center getNotificationSettingsWithCompletionHandler:^(UNNotificationSettings *settings) {
            atomic_store(&replicaroNotificationStatus, (int)settings.authorizationStatus);
        }];
        replicaroOpenRequested();
    }
}

- (void)openReplicaro:(id)sender {
    (void)sender;
    replicaroOpenRequested();
}

- (void)quitReplicaro:(id)sender {
    (void)sender;
    replicaroQuitRequested();
    ReplicaroStopApp();
}

- (NSApplicationTerminateReply)applicationShouldTerminate:(NSApplication *)sender {
    (void)sender;
    replicaroQuitRequested();
    ReplicaroStopApp();
    return NSTerminateCancel;
}

- (BOOL)applicationShouldTerminateAfterLastWindowClosed:(NSApplication *)sender {
    (void)sender;
    return NO;
}

- (void)userNotificationCenter:(UNUserNotificationCenter *)center
 didReceiveNotificationResponse:(UNNotificationResponse *)response
          withCompletionHandler:(void (^)(void))completionHandler {
    (void)center;
    NSString *target = response.notification.request.content.userInfo[@"target"];
    if ([target isKindOfClass:[NSString class]]) {
        NSURL *url = [NSURL URLWithString:target];
        if (url != nil) {
            dispatch_async(dispatch_get_main_queue(), ^{
                [[NSWorkspace sharedWorkspace] openURL:url];
            });
        }
    }
    completionHandler();
}

@end

static ReplicaroAppDelegate *replicaroDelegate;
static atomic_bool replicaroStopRequested = false;

static void replicaroStopRunLoop(void) {
    [NSApp stop:nil];
    NSEvent *wake = [NSEvent otherEventWithType:NSEventTypeApplicationDefined
                                       location:NSZeroPoint
                                  modifierFlags:0
                                      timestamp:0
                                   windowNumber:0
                                        context:nil
                                        subtype:0
                                          data1:0
                                          data2:0];
    [NSApp postEvent:wake atStart:NO];
}

void ReplicaroRunApp(void) {
    @autoreleasepool {
        NSApplication *app = [NSApplication sharedApplication];
        [app setActivationPolicy:NSApplicationActivationPolicyAccessory];
        replicaroDelegate = [[ReplicaroAppDelegate alloc] init];
        app.delegate = replicaroDelegate;
        if (atomic_load(&replicaroStopRequested)) {
            return;
        }
        [app run];
    }
}

void ReplicaroStopApp(void) {
    atomic_store(&replicaroStopRequested, true);
    dispatch_async(dispatch_get_main_queue(), ^{
        replicaroStopRunLoop();
    });
}

int ReplicaroIsMainThread(void) {
    return pthread_main_np() != 0;
}

int ReplicaroOpenURL(const char *target) {
    if (target == NULL) {
        return 0;
    }
    NSString *value = [NSString stringWithUTF8String:target];
    NSURL *url = [NSURL URLWithString:value];
    if (url == nil) {
        return 0;
    }
    dispatch_async(dispatch_get_main_queue(), ^{
        [[NSWorkspace sharedWorkspace] openURL:url];
    });
    return 1;
}

int ReplicaroNotificationCapability(void) {
    return atomic_load(&replicaroNotificationStatus) != UNAuthorizationStatusDenied;
}

int ReplicaroShowNotification(const char *title, const char *message, const char *target, const char *iconPath) {
    if (getenv("REPLICARO_DESKTOP_TEST") != NULL) {
        return 0;
    }
    if (title == NULL || message == NULL || target == NULL) {
        return 0;
    }
    if (!ReplicaroNotificationCapability()) {
        return 0;
    }
    NSString *notificationTitle = [NSString stringWithUTF8String:title];
    NSString *notificationMessage = [NSString stringWithUTF8String:message];
    NSString *notificationTarget = [NSString stringWithUTF8String:target];
    NSString *notificationIcon = iconPath ? [NSString stringWithUTF8String:iconPath] : nil;
    UNUserNotificationCenter *center = [UNUserNotificationCenter currentNotificationCenter];
    [center requestAuthorizationWithOptions:(UNAuthorizationOptionAlert | UNAuthorizationOptionSound)
                          completionHandler:^(BOOL granted, NSError *error) {
        if (!granted || error != nil) {
            atomic_store(&replicaroNotificationStatus, UNAuthorizationStatusDenied);
            if (error != nil) {
                NSLog(@"Replicaro notification authorization failed: %@", error);
            } else {
                NSLog(@"Replicaro notification authorization was denied");
            }
            return;
        }
        atomic_store(&replicaroNotificationStatus, UNAuthorizationStatusAuthorized);
        UNMutableNotificationContent *content = [[UNMutableNotificationContent alloc] init];
        content.title = notificationTitle;
        content.body = notificationMessage;
        content.userInfo = @{@"target": notificationTarget};
        // macOS owns the header's application icon. A supported image attachment
        // provides outcome artwork in the content when the user's presentation
        // permits it. The notification service takes ownership of its file.
        NSString *temporaryIcon = [NSTemporaryDirectory() stringByAppendingPathComponent:
            [[[NSUUID UUID] UUIDString] stringByAppendingPathExtension:@"png"]];
        if (notificationIcon && [[NSFileManager defaultManager] copyItemAtPath:notificationIcon toPath:temporaryIcon error:nil]) {
            UNNotificationAttachment *attachment = [UNNotificationAttachment attachmentWithIdentifier:@"replicaro-outcome"
                URL:[NSURL fileURLWithPath:temporaryIcon] options:nil error:nil];
            if (attachment) content.attachments = @[attachment];
        }
        UNNotificationRequest *request = [UNNotificationRequest requestWithIdentifier:[[NSUUID UUID] UUIDString]
                                                                                  content:content
                                                                                  trigger:nil];
        [center addNotificationRequest:request withCompletionHandler:^(NSError *requestError) {
            // Validation/transfer happens when the request is scheduled, not
            // when the attachment object is created. Retain the file until this
            // callback, then remove any residue after success or failure.
            [[NSFileManager defaultManager] removeItemAtPath:temporaryIcon error:nil];
            if (requestError != nil) {
                NSLog(@"Replicaro notification delivery failed: %@", requestError);
            }
        }];
    }];
    return 1;
}
