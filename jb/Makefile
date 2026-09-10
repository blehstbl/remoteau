THEOS_DEVICE_IP ?=

TARGET := iphone:clang:16.5:16.0
ARCHS = arm64 arm64e

include $(THEOS)/makefiles/common.mk

TWEAK_NAME = RemoteAUCompanion RemoteAUControlCenter

RemoteAUCompanion_FILES = RemoteAUCompanion.xm
RemoteAUCompanion_FRAMEWORKS = UIKit Foundation AVFoundation
RemoteAUCompanion_CFLAGS = -fobjc-arc

# Control Center toggle. It references the private CCUIControlCenterButton class
# from ControlCenterUIKit. The public SDK does not ship that private header, so a
# minimal declaration lives in headers/ and the class symbol is resolved from
# SpringBoard at load time via -Wl,-undefined,dynamic_lookup (no linking against
# the private framework).
RemoteAUControlCenter_FILES = RemoteAUControlCenter.xm
RemoteAUControlCenter_FRAMEWORKS = UIKit Foundation
RemoteAUControlCenter_CFLAGS = -fobjc-arc -I$(THEOS_PROJECT_DIR)/headers
RemoteAUControlCenter_LDFLAGS = -Wl,-undefined,dynamic_lookup

include $(THEOS_MAKE_PATH)/tweak.mk

after-install::
	install.exec "launchctl bootstop gui/501/dev.remoteau.receiver 2>/dev/null || true"
