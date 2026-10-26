//go:build darwin && arm64

// cmd/oap/internal/desktopcmd/urlevents_darwin.m
//
// Receives oap:// links (CFBundleURLTypes in build/desktop/Info.plist) as
// kInternetEventClass/kAEGetURL Apple Events and forwards each URL string to
// Go's goOAPURLEvent (urlevents_darwin.go). fyne.io/systray installs its own
// NSApplication delegate, so application:openURLs: is not available to us;
// an NSAppleEventManager handler needs no delegate. Separate .m file for the
// same reason as activate_darwin.m: Cocoa.h needs a real Objective-C unit,
// and a Go file with an //export cannot carry C definitions in its preamble.

#import <Cocoa/Cocoa.h>
#include "_cgo_export.h"

@interface OAPURLEventHandler : NSObject
- (void)handleGetURLEvent:(NSAppleEventDescriptor *)event
           withReplyEvent:(NSAppleEventDescriptor *)reply;
@end

@implementation OAPURLEventHandler
- (void)handleGetURLEvent:(NSAppleEventDescriptor *)event
           withReplyEvent:(NSAppleEventDescriptor *)reply {
  NSString *url = [[event paramDescriptorForKeyword:keyDirectObject] stringValue];
  if (url == nil) {
    NSLog(@"OAP Desktop: received GetURL event without a URL");
    return;
  }
  const char *s = [url UTF8String];
  if (s == NULL) {
    NSLog(@"OAP Desktop: received GetURL event without a URL");
    return;
  }
  // goOAPURLEvent copies the string (C.GoString) before returning, so the
  // autoreleased UTF8String buffer only needs to outlive this call.
  goOAPURLEvent((char *)s);
}
@end

// Both are held for the process lifetime: NSAppleEventManager does not retain
// its handler object, and the notification observer token must stay alive
// for the block to fire. cgo compiles this file without ARC (manual
// retain/release), so the +1 from alloc/init is the handler's ownership and
// the autoreleased observer token is retained explicitly below.
static OAPURLEventHandler *apURLHandler;
static id apURLWillFinishObserver;

static void apRegisterGetURL(void) {
  [[NSAppleEventManager sharedAppleEventManager]
      setEventHandler:apURLHandler
          andSelector:@selector(handleGetURLEvent:withReplyEvent:)
        forEventClass:kInternetEventClass
           andEventID:kAEGetURL];
}

// apInstallURLHandler registers the GetURL handler now AND again on
// NSApplicationWillFinishLaunchingNotification. -[NSApplication
// finishLaunching] can install AppKit's own GetURL handling, which would
// silently displace a handler registered earlier; re-registering at
// will-finish-launching (before the queued cold-launch event is dispatched)
// puts ours back on top. Idempotent: a second call is a no-op.
void apInstallURLHandler(void) {
  static dispatch_once_t once;
  dispatch_once(&once, ^{
    // Runs before systray.Run starts the run loop, so there is no ambient
    // autorelease pool yet.
    @autoreleasepool {
      apURLHandler = [[OAPURLEventHandler alloc] init];
      apRegisterGetURL();
      apURLWillFinishObserver = [[[NSNotificationCenter defaultCenter]
          addObserverForName:NSApplicationWillFinishLaunchingNotification
                      object:nil
                       queue:nil
                  usingBlock:^(NSNotification *note) {
                    apRegisterGetURL();
                  }] retain];
    }
  });
}
