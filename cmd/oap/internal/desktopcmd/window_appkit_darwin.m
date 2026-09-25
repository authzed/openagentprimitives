//go:build darwin && arm64

// cmd/ap/desktop_window_appkit_darwin.m
//
// Small AppKit helpers for the `oap desktop-window` child process (see
// desktop_window_appkit_darwin.go, called from desktop_window_darwin.go).
// Kept as a separate .m file (compiled by cgo automatically, no explicit
// -x objective-c CFLAGS needed) rather than folded into the .go file's
// cgo preamble, since Cocoa.h needs a real Objective-C compilation unit.

#import <Cocoa/Cocoa.h>

// apWindowActivate forces this process to NSApplicationActivationPolicyRegular
// (Dock icon + Cmd-Tab entry + focusable) and brings nsWindowPtr to the
// front. See the Go wrapper (activateDesktopWindow) for why webview_go's
// own built-in activation dance doesn't fire here.
void apWindowActivate(void *nsWindowPtr) {
  @autoreleasepool {
    NSApplication *app = [NSApplication sharedApplication];

    // setActivationPolicy: must be called before activateIgnoringOtherApps:
    // for activation to actually take effect (same ordering requirement
    // webview_go's own webview.h documents).
    [app setActivationPolicy:NSApplicationActivationPolicyRegular];
    [app activateIgnoringOtherApps:YES];

    if (nsWindowPtr != NULL) {
      NSWindow *window = (__bridge NSWindow *)nsWindowPtr;
      [window makeKeyAndOrderFront:nil];
    }
  }
}

// apInstallEditMenu installs a minimal NSApp main menu: an application menu
// (Quit) plus a standard Edit menu (Undo/Redo/Cut/Copy/Paste/Select All)
// bound to the standard first-responder editing selectors with their usual
// key equivalents. This is what makes Cmd-V (and friends) work in the
// config form's text field — see the Go wrapper (installEditMenu) for why
// a bare WKWebView window has no Edit menu otherwise.
void apInstallEditMenu(void) {
  @autoreleasepool {
    NSApplication *app = [NSApplication sharedApplication];

    NSMenu *mainMenu = [[NSMenu alloc] init];

    // Application menu: just Quit, so the standard Cmd-Q shortcut works.
    NSMenuItem *appMenuItem = [[NSMenuItem alloc] init];
    [mainMenu addItem:appMenuItem];
    NSMenu *appMenu = [[NSMenu alloc] init];
    NSString *appName = [[NSProcessInfo processInfo] processName];
    NSMenuItem *quitItem =
        [[NSMenuItem alloc] initWithTitle:[@"Quit " stringByAppendingString:appName]
                                    action:@selector(terminate:)
                             keyEquivalent:@"q"];
    [appMenu addItem:quitItem];
    [appMenuItem setSubmenu:appMenu];

    // Edit menu: the standard first-responder editing selectors. An
    // uppercase keyEquivalent (Redo's "Z") implicitly requires Shift in
    // addition to the default Command modifier — no explicit
    // keyEquivalentModifierMask needed, matching Apple's own default Edit
    // menu (MainMenu.xib) behavior.
    NSMenuItem *editMenuItem = [[NSMenuItem alloc] init];
    [mainMenu addItem:editMenuItem];
    NSMenu *editMenu = [[NSMenu alloc] initWithTitle:@"Edit"];

    [editMenu addItemWithTitle:@"Undo" action:@selector(undo:) keyEquivalent:@"z"];
    [editMenu addItemWithTitle:@"Redo" action:@selector(redo:) keyEquivalent:@"Z"];
    [editMenu addItem:[NSMenuItem separatorItem]];
    [editMenu addItemWithTitle:@"Cut" action:@selector(cut:) keyEquivalent:@"x"];
    [editMenu addItemWithTitle:@"Copy" action:@selector(copy:) keyEquivalent:@"c"];
    [editMenu addItemWithTitle:@"Paste" action:@selector(paste:) keyEquivalent:@"v"];
    [editMenu addItem:[NSMenuItem separatorItem]];
    [editMenu addItemWithTitle:@"Select All" action:@selector(selectAll:) keyEquivalent:@"a"];

    [editMenuItem setSubmenu:editMenu];

    [app setMainMenu:mainMenu];
  }
}
