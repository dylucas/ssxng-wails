// Menu bar status item for SSXNG.
//
// Every AppKit call is funnelled onto the main thread, because Go's startup
// callback does not necessarily run there and NSStatusItem must be created on
// the main thread.
#import <Cocoa/Cocoa.h>
#include <stdlib.h>
#include <string.h>
#include "tray_darwin.h"

static NSStatusItem *gItem = nil;
static NSMenu *gMenu = nil;

@interface SSXNGTrayTarget : NSObject
- (void)toggle:(id)sender;
- (void)selectMode:(id)sender;
- (void)showWindow:(id)sender;
- (void)quitApp:(id)sender;
@end

@implementation SSXNGTrayTarget

- (void)toggle:(id)sender {
    ssxngTrayEvent(1, NULL);
}

- (void)selectMode:(id)sender {
    NSMenuItem *item = (NSMenuItem *)sender;
    NSString *mode = [item representedObject];
    if (mode == nil) {
        return;
    }
    ssxngTrayEvent(2, (char *)[mode UTF8String]);
}

- (void)showWindow:(id)sender {
    ssxngTrayEvent(3, NULL);
}

- (void)quitApp:(id)sender {
    ssxngTrayEvent(4, NULL);
}

@end

static SSXNGTrayTarget *gTarget = nil;

static NSString *ssxngCString(const char *s) {
    if (s == NULL) {
        return @"";
    }
    return [NSString stringWithUTF8String:s] ?: @"";
}

/* A small template image (paper plane), so the icon adapts to the menu bar
   appearance and to the highlighted state. */
static NSImage *ssxngMakeIcon(void) {
    NSSize size = NSMakeSize(18.0, 18.0);
    NSImage *image = [[NSImage alloc] initWithSize:size];
    [image lockFocus];
    NSBezierPath *path = [NSBezierPath bezierPath];
    [path moveToPoint:NSMakePoint(1.5, 9.0)];
    [path lineToPoint:NSMakePoint(16.5, 15.5)];
    [path lineToPoint:NSMakePoint(11.0, 2.5)];
    [path lineToPoint:NSMakePoint(8.5, 8.0)];
    [path closePath];
    [[NSColor blackColor] setFill];
    [path fill];
    [image unlockFocus];
    [image setTemplate:YES];
    return image;
}

static NSMenuItem *ssxngMakeItem(NSString *title, SEL action) {
    NSMenuItem *item = [[NSMenuItem alloc] initWithTitle:title action:action keyEquivalent:@""];
    item.target = gTarget;
    return item;
}

static void ssxngBuildMenu(NSString *status, NSString *currentMode, NSString *modesSpec, BOOL enabled) {
    if (gItem == nil) {
        return;
    }
    if (gTarget == nil) {
        gTarget = [[SSXNGTrayTarget alloc] init];
    }
    gMenu = [[NSMenu alloc] init];
    [gMenu setAutoenablesItems:NO];

    NSMenuItem *statusItem = [[NSMenuItem alloc] initWithTitle:status action:nil keyEquivalent:@""];
    [statusItem setEnabled:NO];
    [gMenu addItem:statusItem];
    [gMenu addItem:[NSMenuItem separatorItem]];

    [gMenu addItem:ssxngMakeItem(enabled ? @"关闭代理" : @"开启代理", @selector(toggle:))];
    [gMenu addItem:[NSMenuItem separatorItem]];

    for (NSString *pair in [modesSpec componentsSeparatedByString:@"\n"]) {
        if ([pair length] == 0) {
            continue;
        }
        NSArray *parts = [pair componentsSeparatedByString:@"|"];
        if ([parts count] < 2) {
            continue;
        }
        NSString *key = [parts objectAtIndex:0];
        NSString *title = [parts objectAtIndex:1];
        NSMenuItem *item = ssxngMakeItem(title, @selector(selectMode:));
        [item setRepresentedObject:key];
        [item setState:[key isEqualToString:currentMode] ? NSControlStateValueOn : NSControlStateValueOff];
        [gMenu addItem:item];
    }

    [gMenu addItem:[NSMenuItem separatorItem]];
    NSMenuItem *show = ssxngMakeItem(@"显示主窗口", @selector(showWindow:));
    [show setKeyEquivalent:@"o"];
    [gMenu addItem:show];
    [gMenu addItem:ssxngMakeItem(@"退出 SSXNG", @selector(quitApp:))];

    [gItem setMenu:gMenu];
}

int ssxngTrayStart(void) {
    __block int result = 0;
    void (^createBlock)(void) = ^{
        if (gItem != nil) {
            result = 0;
            return;
        }
        NSStatusBar *bar = [NSStatusBar systemStatusBar];
        if (bar == nil) {
            result = 1;
            return;
        }
        gItem = [bar statusItemWithLength:NSVariableStatusItemLength];
        if (gItem == nil) {
            result = 1;
            return;
        }
        [gItem retain];
        NSButton *button = [gItem button];
        if (button != nil) {
            [button setImage:ssxngMakeIcon()];
            [button setToolTip:@"SSXNG"];
        }
        ssxngBuildMenu(@"SSXNG", @"", @"", NO);
    };

    if ([NSThread isMainThread]) {
        createBlock();
    } else {
        dispatch_sync(dispatch_get_main_queue(), createBlock);
    }
    return result;
}

void ssxngTrayRefresh(const char *title, const char *status, const char *currentMode, const char *modesSpec, int enabled) {
    NSString *nsTitle = ssxngCString(title);
    NSString *nsStatus = ssxngCString(status);
    NSString *nsMode = ssxngCString(currentMode);
    NSString *nsModes = ssxngCString(modesSpec);
    BOOL on = enabled != 0;
    dispatch_async(dispatch_get_main_queue(), ^{
        if (gItem == nil) {
            return;
        }
        NSButton *button = [gItem button];
        if (button != nil) {
            [button setToolTip:nsTitle];
        }
        ssxngBuildMenu(nsStatus, nsMode, nsModes, on);
    });
}

void ssxngTrayStop(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        if (gItem == nil) {
            return;
        }
        [[NSStatusBar systemStatusBar] removeStatusItem:gItem];
        [gItem release];
        gItem = nil;
        gMenu = nil;
    });
}

int ssxngSetDockIcon(int visible) {
    __block int result = 0;
    void (^applyBlock)(void) = ^{
        NSApplication *app = [NSApplication sharedApplication];
        if (app == nil) {
            result = 1;
            return;
        }
        NSApplicationActivationPolicy policy = visible ? NSApplicationActivationPolicyRegular
                                                       : NSApplicationActivationPolicyAccessory;
        if (![app setActivationPolicy:policy]) {
            result = 1;
            return;
        }
        if (visible) {
            [app activateIgnoringOtherApps:YES];
        }
    };
    if ([NSThread isMainThread]) {
        applyBlock();
    } else {
        dispatch_sync(dispatch_get_main_queue(), applyBlock);
    }
    return result;
}
