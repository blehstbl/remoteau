// RemoteAU Companion tweak (Phase 13, optional — the core IPA never needs
// this). Build with Theos: make package FINALPACKAGE=1.
//
// Features:
//  - AirPods-aware receiver control: signal the RemoteAU app when BT
//    routes connect/disconnect.
//  - SpringBoard status indicator while a PC is streaming.

#import <UIKit/UIKit.h>
#import <Foundation/Foundation.h>
#import <AVFoundation/AVFoundation.h>

#define RA_SIGNAL_PATH @"/var/mobile/Library/Preferences/dev.remoteau.companion"

static void raWriteFlag(BOOL on) {
    NSString *value = on ? @"1" : @"0";
    [[NSFileManager defaultManager] createFileAtPath:RA_SIGNAL_PATH
                                            contents:[value dataUsingEncoding:NSUTF8StringEncoding]
                                          attributes:nil];
}

// Route-change hooks: the exact BT APIs vary by jailbreak/SDK; this sketch
// listens for CoreBluetooth + AVAudioSession route events and signals the
// receiver agent (started via the LaunchDaemon above).
%ctor {
    NSNotificationCenter *nc = [NSNotificationCenter defaultCenter];

    [nc addObserverForName:AVAudioSessionRouteChangeNotification
                    object:nil queue:[NSOperationQueue mainQueue]
                usingBlock:^(NSNotification *note) {
                    AVAudioSessionRouteChangeReason reason =
                        (AVAudioSessionRouteChangeReason)[note.userInfo[AVAudioSessionRouteChangeReasonKey] unsignedIntegerValue];
                    AVAudioSession *session = [AVAudioSession sharedInstance];
                    BOOL hasBT = NO;
                    for (AVAudioSessionPortDescription *out in session.currentRoute.outputs) {
                        if ([out.portType hasPrefix:@"Bluetooth"]) { hasBT = YES; break; }
                    }
                    if (reason == AVAudioSessionRouteChangeReasonNewDeviceAvailable && hasBT) {
                        raWriteFlag(YES);   // AirPods connected: keep receiver running
                    } else if (reason == AVAudioSessionRouteChangeReasonOldDeviceUnavailable && !hasBT) {
                        raWriteFlag(NO);    // AirPods gone: pause receiver
                    }
                }];
}
