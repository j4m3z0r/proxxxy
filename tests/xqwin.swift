// Lists on-screen windows owned by XQuartz/X11 with their CoreGraphics window
// IDs, so we can screen-capture the ACTUAL displayed content (what the user
// sees) regardless of macOS Space — unlike xwd, which reads XQuartz's internal
// X framebuffer and can disagree with the screen.
import CoreGraphics
import Foundation

let opts: CGWindowListOption = [.optionOnScreenOnly, .excludeDesktopElements]
guard let list = CGWindowListCopyWindowInfo(opts, kCGNullWindowID) as? [[String: Any]] else {
    FileHandle.standardError.write("no window list\n".data(using: .utf8)!)
    exit(1)
}
for info in list {
    let owner = info[kCGWindowOwnerName as String] as? String ?? ""
    let name = info[kCGWindowName as String] as? String ?? ""
    let num = info[kCGWindowNumber as String] as? Int ?? 0
    let b = info[kCGWindowBounds as String] as? [String: Any] ?? [:]
    let w = Int(b["Width"] as? Double ?? 0)
    let h = Int(b["Height"] as? Double ?? 0)
    let lo = owner.lowercased()
    if lo.contains("x11") || lo.contains("xquartz") || lo.contains("quartz") {
        print("\(num)\t\(w)x\(h)\t[\(owner)]\t\(name)")
    }
}
