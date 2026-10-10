#!/usr/bin/env bash
# 菜单栏自检：把真实的 internal/tray/tray_darwin.m 编译成一个无界面小程序，
# 断言"复制命令行代理"菜单项确实被建出来、标题正确、并且 representedObject
# 正是要写进剪贴板的那串命令。
#
#   scripts/tray-check.sh            # 检查
#   SSXNG_TRAY_VERBOSE=1 ...         # 打印每一条通过项
#
# 为什么值得单独检查：菜单项是原生代码，Go 的单元测试碰不到它；而它坏掉的方式
# 是静默的（标题空、复制出错位的文字），界面自检也看不见。
#
# 命令文本必须与 frontend/src/app-views.js 的 cliProxyCommands() 以及
# tray_test.go 里的字面量逐字一致——三处各自钉住同一串文字。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$ROOT/internal/tray"

if [ "$(uname -s)" != "Darwin" ]; then
  echo "跳过菜单栏自检：仅 macOS 可用。" >&2
  exit 0
fi
if ! command -v clang >/dev/null 2>&1; then
  echo "跳过菜单栏自检：找不到 clang（需要 Xcode Command Line Tools）。" >&2
  exit 0
fi

TMP="$(mktemp -d /tmp/ssxng-traycheck-XXXXXX)"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

cp "$SRC/tray_darwin.h" "$SRC/tray_darwin.m" "$TMP/"

cat > "$TMP/main.m" <<'OBJC'
#import <Cocoa/Cocoa.h>
#include <stdlib.h>
#include <string.h>
#include "tray_darwin.h"

/* The Go side of the bridge; the menu items never fire here. */
void ssxngTrayEvent(int kind, char *mode) {
    (void)kind;
    (void)mode;
}

/* Pull in the real implementation, so the assertions below cover the shipped
   code rather than a copy of it. */
#include "tray_darwin.m"

static int gChecks = 0;
static int gFailures = 0;

static void check(int ok, NSString *what) {
    gChecks++;
    if (ok) {
        if (getenv("SSXNG_TRAY_VERBOSE") != NULL) {
            printf("  ✓ %s\n", [[what description] UTF8String]);
        }
        return;
    }
    gFailures++;
    printf("  ✗ %s\n", [[what description] UTF8String]);
}

/* The very text the window's 命令行代理 block shows (frontend/src/app-views.js). */
static NSString *const kHTTPCommand =
    @"export http_proxy=http://127.0.0.1:1087;export https_proxy=http://127.0.0.1:1087;";
static NSString *const kSOCKSCommand =
    @"export http_proxy=socks5://127.0.0.1:1086;export https_proxy=socks5://127.0.0.1:1086;";

static NSString *specForCommands(NSArray *lines) {
    return [lines componentsJoinedByString:@"\n"];
}

/* ---- 图标：开启实心 / 关闭空心 ---------------------------------------- */

/* 取图像里某个点的 alpha。坐标用图标自身的 18x18 空间（原点在左下，也就是
   bezier 路径被画进的那个坐标系）；位图的行是从上往下数，所以 y 要翻转。 */
static CGFloat alphaAtPoint(NSImage *image, CGFloat x, CGFloat y) {
    NSBitmapImageRep *rep = [NSBitmapImageRep imageRepWithData:[image TIFFRepresentation]];
    if (rep == nil || [rep pixelsWide] <= 0) {
        return -1.0;
    }
    CGFloat scale = (CGFloat)[rep pixelsWide] / 18.0;
    NSInteger px = (NSInteger)floor(x * scale);
    NSInteger py = (NSInteger)floor((18.0 - y) * scale);
    if (px < 0) px = 0;
    if (py < 0) py = 0;
    if (px >= [rep pixelsWide]) px = [rep pixelsWide] - 1;
    if (py >= [rep pixelsHigh]) py = [rep pixelsHigh] - 1;
    NSColor *color = [rep colorAtX:px y:py];
    return color == nil ? -1.0 : [color alphaComponent];
}

/* 飞机重心（顶点均值）落在多边形内部（射线法验证过），所以它是"实心/空心"最
   直接的判据：实心图那里不透明，空心图那里是空的。 */
static const CGFloat kInsideX = 9.375;
static const CGFloat kInsideY = 8.75;
/* 边 A→B 的中点，正好落在路径上：两种图标在这里都该有笔画。 */
static const CGFloat kOnPathX = 9.0;
static const CGFloat kOnPathY = 12.25;

/* 预览底图上半是浅色、下半是深色。返回"笔画"像素在该半区里的占比：浅色条数深色
   像素，深色条数浅色像素。用来确认图标真的画上去了，而不是导出了两张空白底。 */
static CGFloat inkRatio(NSBitmapImageRep *rep, BOOL lightStrip) {
    if (rep == nil || [rep pixelsHigh] == 0) {
        return -1.0;
    }
    NSInteger half = [rep pixelsHigh] / 2;
    NSInteger y0 = lightStrip ? 0 : half;  // 位图第 0 行在顶部，也就是浅色条
    NSInteger ink = 0, total = 0;
    for (NSInteger y = y0; y < y0 + half; y++) {
        for (NSInteger x = 0; x < [rep pixelsWide]; x++) {
            NSColor *color = [[rep colorAtX:x y:y] colorUsingColorSpace:[NSColorSpace deviceRGBColorSpace]];
            if (color == nil) {
                continue;
            }
            CGFloat lum = 0.299 * [color redComponent] +
                          0.587 * [color greenComponent] +
                          0.114 * [color blueComponent];
            total++;
            if (lightStrip ? (lum < 0.5) : (lum > 0.5)) {
                ink++;
            }
        }
    }
    return total == 0 ? -1.0 : (CGFloat)ink / (CGFloat)total;
}

/* 图标是"黑 + alpha"的 template 图，要靠系统按菜单栏外观重绘。预览里要在深色底上
   模拟那次重绘，就得只把**图标像素**染白：先画进一张透明底的新图，再 SourceAtop
   填白。直接在画布上 SourceAtop 会把不透明的底色一起刷白，糊出一个白方块——
   这正是第一版预览导出的样子。 */
static NSImage *ssxngTintedIcon(NSImage *icon, NSColor *color) {
    NSSize size = [icon size];
    NSImage *out = [[NSImage alloc] initWithSize:size];
    [out lockFocus];
    [icon drawInRect:NSMakeRect(0, 0, size.width, size.height)
            fromRect:NSZeroRect
           operation:NSCompositingOperationSourceOver
            fraction:1.0
      respectFlipped:NO
               hints:nil];
    [color setFill];
    NSRectFillUsingOperation(NSMakeRect(0, 0, size.width, size.height),
                             NSCompositingOperationSourceAtop);
    [out unlockFocus];
    return out;
}

static void writeIconPreviews(NSString *dir) {    CGFloat scale = 6.0;
    CGFloat side = 18.0 * scale;
    CGFloat pad = 18.0;
    NSSize canvasSize = NSMakeSize(side * 2 + pad * 3, side * 2 + pad * 3);
    NSImage *canvas = [[NSImage alloc] initWithSize:canvasSize];
    [canvas lockFocus];

    // 上排浅色、下排深色，模拟两种菜单栏外观。template 图在深色栏上由系统画成
    // 白色，这里手工用 SourceAtop 重新着色，尽量贴近真实观感。
    [[NSColor colorWithCalibratedWhite:0.96 alpha:1.0] setFill];
    NSRectFill(NSMakeRect(0, canvasSize.height / 2, canvasSize.width, canvasSize.height / 2));
    [[NSColor colorWithCalibratedWhite:0.13 alpha:1.0] setFill];
    NSRectFill(NSMakeRect(0, 0, canvasSize.width, canvasSize.height / 2));

    NSArray *icons = @[ ssxngIconForEnabled(YES), ssxngIconForEnabled(NO) ];
    // AppKit 的原点在左下：先填的 y=h/2..h 那半边在成品图里是**上**半部分。
    // row 0 画在上半边，所以它是浅色底（深色底那排必须是 row 1）。
    for (int row = 0; row < 2; row++) {
        BOOL dark = (row == 1);
        for (int col = 0; col < 2; col++) {
            NSImage *icon = [icons objectAtIndex:col];  // 显式类型，避免 id 上的方法二义性
            if (dark) {
                icon = ssxngTintedIcon(icon, [NSColor whiteColor]);
            }
            NSRect r = NSMakeRect(pad + col * (side + pad),
                                  (dark ? 0 : canvasSize.height / 2) + pad,
                                  side, side);
            [icon drawInRect:r
                    fromRect:NSZeroRect
                   operation:NSCompositingOperationSourceOver
                    fraction:1.0
              respectFlipped:NO
                       hints:nil];
        }
    }
    [canvas unlockFocus];

    /* 预览是给人看的，画错也不会有人报错（图标本身是对的），所以这里自己确认
       一遍：两条底色上都要有笔画，而且占比得落在"纸飞机"的量级里。
       上界专门用来抓第一版那个 bug——SourceAtop 把不透明底色也刷白，于是整块
       图标框变成白方块，占比冲到 0.6 以上。 */
    NSBitmapImageRep *canvasRep = [NSBitmapImageRep imageRepWithData:[canvas TIFFRepresentation]];
    CGFloat lightInk = inkRatio(canvasRep, YES);
    CGFloat darkInk = inkRatio(canvasRep, NO);
    check(lightInk > 0.03 && lightInk < 0.30,
          [NSString stringWithFormat:@"预览浅色条上只有纸飞机大小的深色笔画（占比 %.4f）", lightInk]);
    check(darkInk > 0.03 && darkInk < 0.30,
          [NSString stringWithFormat:@"预览深色条上只有纸飞机大小的浅色笔画（占比 %.4f）", darkInk]);

    NSArray *names = @[ @"icon-on.png", @"icon-off.png", @"icon-preview.png" ];
    NSArray *images = @[ icons[0], icons[1], canvas ];
    for (NSUInteger i = 0; i < [names count]; i++) {
        NSBitmapImageRep *rep =
            [NSBitmapImageRep imageRepWithData:[images[i] TIFFRepresentation]];
        NSData *png = [rep representationUsingType:NSBitmapImageFileTypePNG properties:@{}];
        NSString *path = [dir stringByAppendingPathComponent:names[i]];
        if (png != nil) {
            [png writeToFile:path atomically:YES];
            printf("  · 图标预览: %s\n", [path UTF8String]);
        }
    }
}

static void checkIcons(void) {
    NSImage *on = ssxngIconForEnabled(YES);
    NSImage *off = ssxngIconForEnabled(NO);

    check(on != nil && off != nil, @"开启/关闭两种状态都拿得到图标");
    check(on != off, @"开启与关闭的图标不是同一张图");
    check(NSEqualSizes([on size], NSMakeSize(18.0, 18.0)) &&
              NSEqualSizes([off size], NSMakeSize(18.0, 18.0)),
          @"两张图标都是 18x18");
    // template 图只按 alpha 渲染，菜单栏深浅色与高亮反色都由系统处理。
    check([on isTemplate] && [off isTemplate], @"两张图标都是 template（跟随菜单栏深浅色）");

    CGFloat onInside = alphaAtPoint(on, kInsideX, kInsideY);
    CGFloat offInside = alphaAtPoint(off, kInsideX, kInsideY);
    check(onInside > 0.5, @"开启图标在飞机重心处是实心的");
    check(offInside < 0.5, @"关闭图标在飞机重心处是空心的");

    CGFloat onEdge = alphaAtPoint(on, kOnPathX, kOnPathY);
    CGFloat offEdge = alphaAtPoint(off, kOnPathX, kOnPathY);
    check(onEdge > 0.5 && offEdge > 0.5, @"两张图标在轮廓上都画出了笔画（关闭不是一张空图）");

    // 缓存：每次刷新都会取一次图标，不该每取一次就重画。
    check(ssxngIconForEnabled(YES) == on && ssxngIconForEnabled(NO) == off,
          @"图标被缓存（重复取用返回同一个对象）");
}

int main(void) {
    @autoreleasepool {
        // ssxngCommandItems routes items through ssxngMakeItem, which stamps the
        // shared target; ssxngBuildMenu normally creates it.
        gTarget = [[SSXNGTrayTarget alloc] init];

        // 1) 两个监听都开启：HTTP + SOCKS5 两条，顺序与界面一致
        NSString *both = specForCommands(@[
            [NSString stringWithFormat:@"http|复制 HTTP 代理命令|%@", kHTTPCommand],
            [NSString stringWithFormat:@"socks|复制 SOCKS5 代理命令|%@", kSOCKSCommand],
        ]);
        NSArray *items = ssxngCommandItems(both);
        check([items count] == 2, @"HTTP + SOCKS5 生成 2 个菜单项");
        if ([items count] == 2) {
            NSMenuItem *http = [items objectAtIndex:0];
            NSMenuItem *socks = [items objectAtIndex:1];
            check([[http title] isEqualToString:@"复制 HTTP 代理命令"], @"第一项是 HTTP 且标题正确");
            check([[socks title] isEqualToString:@"复制 SOCKS5 代理命令"], @"第二项是 SOCKS5 且标题正确");
            check([[http representedObject] isEqualToString:kHTTPCommand],
                  @"HTTP 项携带的命令与界面逐字一致");
            check([[socks representedObject] isEqualToString:kSOCKSCommand],
                  @"SOCKS5 项携带的命令与界面逐字一致");
            check([http action] == @selector(copyCommand:), @"HTTP 项点击即复制（copyCommand:）");
            check([socks action] == @selector(copyCommand:), @"SOCKS5 项点击即复制（copyCommand:）");
            check([http target] == gTarget && [socks target] == gTarget, @"菜单项目标指向托盘对象");
            check(![http isSeparatorItem] && ![socks isSeparatorItem], @"命令项不是分隔线");
            check([http isEnabled], @"命令项可点击");
        }

        // 2) HTTP 关闭：只剩 SOCKS5（与界面隐藏该行一致）
        NSString *socksOnly = specForCommands(@[
            [NSString stringWithFormat:@"socks|复制 SOCKS5 代理命令|%@", kSOCKSCommand],
        ]);
        NSArray *one = ssxngCommandItems(socksOnly);
        check([one count] == 1 &&
                  [[[one objectAtIndex:0] representedObject] isEqualToString:kSOCKSCommand],
              @"HTTP 关闭时只剩 SOCKS5 一项");

        // 3) 空 spec：整段复制菜单不出现（不是留一个空项）
        check([ssxngCommandItems(@"") count] == 0, @"空 spec 不生成任何菜单项");

        // 4) 坏行被跳过，不会变成空白菜单项
        check([ssxngCommandItems(@"没有分隔符\n只有一个|") count] == 0, @"坏行被跳过");

        // 5) 命令里出现 "|" 也不能被截断
        NSArray *piped = ssxngCommandItems(@"http|复制 HTTP 代理命令|a|b|c");
        check([piped count] == 1 &&
                  [[[piped objectAtIndex:0] representedObject] isEqualToString:@"a|b|c"],
              @"命令里的竖线不会被截断");

        // 6) 图标随开关变化：开启实心，关闭空心
        checkIcons();
        const char *out = getenv("SSXNG_TRAY_ICON_OUT");
        if (out != NULL) {
            writeIconPreviews([NSString stringWithUTF8String:out]);
        }
    }

    if (gFailures == 0) {
        printf("菜单栏自检通过：%d 项检查。\n", gChecks);
        return 0;
    }
    printf("菜单栏自检失败：%d/%d 项。\n", gFailures, gChecks);
    return 1;
}
OBJC

# 与 cgo 一致：不使用 ARC（tray_darwin.m 里有显式 retain/release）。
clang -fno-objc-arc -framework Cocoa -framework AppKit -framework Foundation \
    -I "$TMP" -o "$TMP/tray-check" "$TMP/main.m"

"$TMP/tray-check"
