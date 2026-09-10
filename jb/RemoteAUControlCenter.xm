// RemoteAU Control Center toggle tweak (Phase 13 companion, OPTIONAL).
//
// This is a SEPARATE tweak from RemoteAUCompanion.xm so the AirPods route-aware
// logic stays small. It adds a single Control Center button that toggles the
// RemoteAU receiver by writing the same signal file the AirPods logic uses:
//
//     /var/mobile/Library/Preferences/dev.remoteau.companion   ("YES" / "NO")
//
// The button NEVER spawns a receiver itself. It only flips the signal that the
// existing LaunchDaemon / receiver agent observes, so there is still exactly
// one receiver process.
//
// IMPORTANT / UNVERIFIED: registering a button in modern Control Center needs
// the private ControlCenterServices module system (a module bundle plus the
// module whitelist / provider registration), not a plain plist. That schema
// could not be verified here, so this tweak instead injects a
// CCUIControlCenterButton subclass into whichever Control Center view
// controller appears. See jb/README.md for the caveats. Build with Theos:
//
//     make package FINALPACKAGE=1

#import <UIKit/UIKit.h>
#import <Foundation/Foundation.h>
#import <dispatch/dispatch.h>
#import <ControlCenterUIKit/CCUIControlCenterButton.h>

static NSString *const kRemoteAUSignalPath = @"/var/mobile/Library/Preferences/dev.remoteau.companion";

#pragma mark - Signal file (shared with the AirPods logic)

static BOOL RAControlCenterReadSignal(void) {
    NSString *value = [NSString stringWithContentsOfFile:kRemoteAUSignalPath
                                                encoding:NSUTF8StringEncoding
                                                   error:NULL];
    value = [value stringByTrimmingCharactersInSet:[NSCharacterSet whitespaceAndNewlineCharacterSet]];
    return [value isEqualToString:@"YES"];
}

static void RAControlCenterWriteSignal(BOOL on) {
    NSData *data = [(on ? @"YES" : @"NO") dataUsingEncoding:NSUTF8StringEncoding];
    [[NSFileManager defaultManager] createFileAtPath:kRemoteAUSignalPath
                                            contents:data
                                          attributes:nil];
}

static UIImage *RAControlCenterGlyph(void) {
    UIImage *image = [UIImage imageNamed:@"RemoteAU"];
    if (!image) {
        image = [UIImage imageNamed:@"RemoteAUControlCenter"];
    }
    if (!image) {
        if (@available(iOS 13.0, *)) {
            image = [UIImage systemImageNamed:@"headphones"];
        }
    }
    if (!image) {
        image = [UIImage imageNamed:@"AirPlay"];
    }
    return image;
}

#pragma mark - Control Center button

@interface RemoteAUControlCenterButton : CCUIControlCenterButton
- (void)raRefreshFromSignal;
@end

@implementation RemoteAUControlCenterButton

- (instancetype)init {
    return [self initWithFrame:CGRectZero];
}

- (instancetype)initWithFrame:(CGRect)frame {
    self = [super initWithFrame:frame];
    if (self) {
        UIImage *glyph = RAControlCenterGlyph();
        if (glyph) {
            self.glyphImage = glyph;
            self.selectedGlyphImage = glyph;
            [self setImage:glyph forState:UIControlStateNormal];
            [self setImage:glyph forState:UIControlStateSelected];
        }
        [self addTarget:self
                 action:@selector(raTapped:)
       forControlEvents:UIControlEventTouchUpInside];
        [self raRefreshFromSignal];
    }
    return self;
}

- (void)raRefreshFromSignal {
    self.selected = RAControlCenterReadSignal();
}

- (void)raTapped:(id)sender {
    BOOL on = !RAControlCenterReadSignal();
    RAControlCenterWriteSignal(on);
    self.selected = on;
}

@end

#pragma mark - Best-effort installer

@interface RemoteAUControlCenterInstaller : NSObject
+ (instancetype)sharedInstance;
- (void)installInView:(UIView *)view;
@end

@implementation RemoteAUControlCenterInstaller {
    RemoteAUControlCenterButton *_button;
}

+ (instancetype)sharedInstance {
    static RemoteAUControlCenterInstaller *shared = nil;
    static dispatch_once_t onceToken;
    dispatch_once(&onceToken, ^{
        shared = [[RemoteAUControlCenterInstaller alloc] init];
    });
    return shared;
}

- (void)installInView:(UIView *)view {
    if (!view) {
        return;
    }
    if (!_button) {
        _button = [[RemoteAUControlCenterButton alloc] initWithFrame:CGRectMake(0, 0, 44, 44)];
        _button.autoresizingMask = UIViewAutoresizingFlexibleLeftMargin | UIViewAutoresizingFlexibleTopMargin;
    }
    [_button raRefreshFromSignal];

    if (_button.superview == view) {
        return;
    }
    [_button removeFromSuperview];

    CGRect bounds = view.bounds;
    CGFloat size = 44.0;
    _button.frame = CGRectMake(bounds.size.width - size - 16.0,
                               bounds.size.height - size - 16.0,
                               size, size);
    [view addSubview:_button];
}

@end

static BOOL RAControlCenterIsControlCenter(UIViewController *controller) {
    NSString *name = NSStringFromClass([controller class]);
    if (name.length == 0) {
        return NO;
    }
    if ([name hasPrefix:@"CCUI"]) {
        return YES;
    }
    return ([name rangeOfString:@"ControlCenter"].location != NSNotFound);
}

// UIViewController is a public class, so this hook compiles even without private
// headers. We only act for Control Center view controllers.
%hook UIViewController

- (void)viewDidAppear:(BOOL)animated {
    %orig;
    if (RAControlCenterIsControlCenter(self)) {
        [[RemoteAUControlCenterInstaller sharedInstance] installInView:self.view];
    }
}

%end
