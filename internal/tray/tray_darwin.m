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
- (void)copyCommand:(id)sender;
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

/* Write the command straight to the pasteboard. Doing it here (instead of
   round-tripping through the frontend) keeps the menu item working when the
   window is hidden or the webview is not up yet; the event is only a
   notification so Go can log it and toast. */
- (void)copyCommand:(id)sender {
    NSMenuItem *item = (NSMenuItem *)sender;
    NSString *command = [item representedObject];
    if (command == nil || [command length] == 0) {
        return;
    }
    NSPasteboard *pasteboard = [NSPasteboard generalPasteboard];
    [pasteboard clearContents];
    [pasteboard setString:command forType:NSPasteboardTypeString];
    ssxngTrayEvent(5, (char *)[command UTF8String]);
}

@end

static SSXNGTrayTarget *gTarget = nil;

static NSString *ssxngCString(const char *s) {
    if (s == NULL) {
        return @"";
    }
    return [NSString stringWithUTF8String:s] ?: @"";
}

/* The status item icon: an 18x18 template paper plane. A template image is
   rendered by macOS from its alpha channel alone, so it follows the menu bar
   appearance (black on a light bar, white on a dark one) and inverts when the
   item is highlighted — which is exactly why the on/off difference has to live
   in the shape rather than in a colour.

   On  = filled silhouette.
   Off = the same silhouette, stroked and unfilled, so it reads as "hollow".

   Both are cached for the lifetime of the process: the plane never changes, and
   Refresh runs on every state change. The globals hold the +1 from alloc, the
   same way gItem/gMenu do. */
static NSImage *gIconOn = nil;
static NSImage *gIconOff = nil;

static NSImage *ssxngMakeIcon(BOOL enabled) {
    NSSize size = NSMakeSize(18.0, 18.0);
    NSImage *image = [[NSImage alloc] initWithSize:size];
    [image lockFocus];
    NSBezierPath *path = [NSBezierPath bezierPath];
    [path moveToPoint:NSMakePoint(1.5, 9.0)];
    [path lineToPoint:NSMakePoint(16.5, 15.5)];
    [path lineToPoint:NSMakePoint(11.0, 2.5)];
    [path lineToPoint:NSMakePoint(8.5, 8.0)];
    [path closePath];
    if (enabled) {
        [[NSColor blackColor] setFill];
        [path fill];
    } else {
        // Round joins keep the two sharp corners of the plane from spiking; the
        // stroke straddles the path, so it still fits inside the 18x18 box.
        [path setLineWidth:1.5];
        [path setLineJoinStyle:NSLineJoinStyleRound];
        [[NSColor blackColor] setStroke];
        [path stroke];
    }
    [image unlockFocus];
    [image setTemplate:YES];
    return image;
}

/* The icon to show for a given proxy state. Split out so the check script can
   assert the two states really do produce different images. */
static NSImage *ssxngIconForEnabled(BOOL enabled) {
    NSImage **slot = enabled ? &gIconOn : &gIconOff;
    if (*slot == nil) {
        *slot = ssxngMakeIcon(enabled);
    }
    return *slot;
}

static NSMenuItem *ssxngMakeItem(NSString *title, SEL action) {
    NSMenuItem *item = [[NSMenuItem alloc] initWithTitle:title action:action keyEquivalent:@""];
    item.target = gTarget;
    return item;
}

/* One "key|title|command" line per copy item. Everything after the second
   separator is the command, so a command containing "|" still survives. */
static NSArray *ssxngParseCommand(NSString *line) {
    NSArray *parts = [line componentsSeparatedByString:@"|"];
    if ([parts count] < 3) {
        return nil;
    }
    NSArray *rest = [parts subarrayWithRange:NSMakeRange(2, [parts count] - 2)];
    return @[ [parts objectAtIndex:1], [rest componentsJoinedByString:@"|"] ];
}

/* The copy-command menu items. Each one carries its text as representedObject,
   which is exactly what copyCommand: writes to the pasteboard. Split out from
   ssxngBuildMenu so it can be exercised without a status bar; the assertions
   live in scripts/tray-check.sh. */
static NSArray *ssxngCommandItems(NSString *commandsSpec) {
    NSMutableArray *items = [NSMutableArray array];
    for (NSString *line in [commandsSpec componentsSeparatedByString:@"\n"]) {
        if ([line length] == 0) {
            continue;
        }
        NSArray *parsed = ssxngParseCommand(line);
        if (parsed == nil) {
            continue;
        }
        NSMenuItem *item = ssxngMakeItem([parsed objectAtIndex:0], @selector(copyCommand:));
        [item setRepresentedObject:[parsed objectAtIndex:1]];
        [items addObject:item];
    }
    return items;
}

static void ssxngBuildMenu(NSString *status, NSString *currentMode, NSString *modesSpec,
                           NSString *commandsSpec, BOOL enabled) {
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

    /* Copy-command items. They are ordinary items (one click, no submenu) so the
       text lands on the pasteboard the moment the user picks it. */
    NSArray *commandItems = ssxngCommandItems(commandsSpec);
    if ([commandItems count] > 0) {
        [gMenu addItem:[NSMenuItem separatorItem]];
        for (NSMenuItem *item in commandItems) {
            [gMenu addItem:item];
        }
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
            // Off until a Refresh says otherwise; ssxngTrayStart calls one right
            // away, so this only covers the instant before it lands.
            [button setImage:ssxngIconForEnabled(NO)];
            [button setToolTip:@"SSXNG"];
        }
        ssxngBuildMenu(@"SSXNG", @"", @"", @"", NO);
    };

    if ([NSThread isMainThread]) {
        createBlock();
    } else {
        dispatch_sync(dispatch_get_main_queue(), createBlock);
    }
    return result;
}

void ssxngTrayRefresh(const char *title, const char *status, const char *currentMode, const char *modesSpec, const char *commandsSpec, int enabled) {
    NSString *nsTitle = ssxngCString(title);
    NSString *nsStatus = ssxngCString(status);
    NSString *nsMode = ssxngCString(currentMode);
    NSString *nsModes = ssxngCString(modesSpec);
    NSString *nsCommands = ssxngCString(commandsSpec);
    BOOL on = enabled != 0;
    dispatch_async(dispatch_get_main_queue(), ^{
        if (gItem == nil) {
            return;
        }
        NSButton *button = [gItem button];
        if (button != nil) {
            // 代理开关一变，菜单栏图标就跟着变：实心=开启，空心=关闭。
            [button setImage:ssxngIconForEnabled(on)];
            [button setToolTip:nsTitle];
        }
        ssxngBuildMenu(nsStatus, nsMode, nsModes, nsCommands, on);
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
