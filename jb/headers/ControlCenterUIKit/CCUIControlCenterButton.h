// Minimal declaration of the private ControlCenterUIKit button class.
//
// Theos' target SDK (iPhoneOS, no private headers) does not ship
// ControlCenterUIKit headers, and the CI builds in .github/workflows/
// build-jb.yml only install the public iPhoneOS16.5 SDK. RemoteAUControlCenter.xm
// needs the class only at compile time; at runtime the real class is provided by
// ControlCenterUIKit inside SpringBoard. The tweak links with
// -Wl,-undefined,dynamic_lookup so the real symbol resolves when SpringBoard
// loads the dylib.
//
// Only the members the tweak uses are declared. If a full private header tree
// is ever available, this file is shadowed by the SDK/vendor include path.
#import <UIKit/UIKit.h>

@interface CCUIControlCenterButton : UIButton

@property (nonatomic, retain) UIImage *glyphImage;
@property (nonatomic, retain) UIImage *selectedGlyphImage;

- (void)setGlyphImage:(UIImage *)glyphImage
   selectedGlyphImage:(UIImage *)selectedGlyphImage
                 name:(NSString *)name;

@end
