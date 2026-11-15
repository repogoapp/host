// The Swift half of simcap's Universal HID: finding a simulator's HID
// capability through CoreDevice, whose getImplementation is a Swift async
// function C can't call. Built by scripts/build-simcap-uhid.sh into a static
// library that builds tagged simcap_uhid link. Adapted from serve-sim's
// CoreDeviceBridge.swift (Apache-2.0, https://github.com/expo/serve-sim).
import Foundation
import SimcapUHID

/// A device's capability, by its type's metadata and conformance symbols, or
/// nil after 10 seconds. Blocks the calling thread, which must not be one of
/// Swift's concurrency threads; simcap calls it once per capability at start.
@_cdecl("simcap_uhid_capability")
public func capability(_ udid: UnsafePointer<CChar>, _ metadata: UnsafePointer<CChar>,
                       _ witness: UnsafePointer<CChar>) -> UnsafeMutableRawPointer? {
  let request = (String(cString: udid), String(cString: metadata), String(cString: witness))
  let done = DispatchSemaphore(value: 0)
  let found = Found()
  Task.detached {
    found.storage = try? await CoreDeviceBridge.capability(udid: request.0, metadata: request.1, witness: request.2)
    done.signal()
  }
  return done.wait(timeout: .now() + 10) == .success ? found.storage : nil
}

private final class Found: @unchecked Sendable {
  var storage: UnsafeMutableRawPointer?
}

/// The active panel's screen ID and its framebuffer's rotation in degrees;
/// false when CoreDevice can't say within two seconds. simcap polls it to
/// follow a foldable's panels and any device's turns.
@_cdecl("simcap_uhid_display")
public func display(_ udid: UnsafePointer<CChar>, _ screen: UnsafeMutablePointer<UInt32>,
                    _ rotation: UnsafeMutablePointer<Int32>) -> Bool {
  let id = String(cString: udid)
  let done = DispatchSemaphore(value: 0)
  let found = FoundDisplay()
  Task.detached {
    found.display = try? await CoreDeviceBridge.activeDisplay(udid: id)
    done.signal()
  }
  guard done.wait(timeout: .now() + 2) == .success, let active = found.display else { return false }
  screen.pointee = active.screen
  rotation.pointee = active.rotation
  return true
}

private final class FoundDisplay: @unchecked Sendable {
  var display: (screen: UInt32, rotation: Int32)?
}

/// Sends a vendor control through capability: data is a CFData Device Hub's
/// serializer made, whose two-word Foundation.Data form the call takes.
@_cdecl("simcap_uhid_control")
public func control(_ capability: UnsafeMutableRawPointer?, _ data: UnsafeMutableRawPointer?) -> Bool {
  guard let capability, let data else { return false }
  let bytes = Unmanaged<NSData>.fromOpaque(data).takeRetainedValue() as Data
  let words = unsafeBitCast(bytes, to: (UInt64, UInt64).self)
  return withExtendedLifetime(bytes) { SSCoreDeviceSendControl(capability, words.0, words.1) }
}

private enum CoreDeviceBridge {
  enum Failure: Error { case unavailable, notInitialized, noDevice }

  nonisolated(unsafe) private static var manager: CoreDeviceManagerObject?

  static func capability(udid: String, metadata metadataSymbol: String, witness witnessSymbol: String) async throws
    -> UnsafeMutableRawPointer
  {
    let device = try await remoteDevice(udid: udid)
    guard let metadata = SSCoreDeviceSymbol(metadataSymbol), let witness = SSCoreDeviceSymbol(witnessSymbol) else {
      throw Failure.unavailable
    }
    // An existential's worth of storage, kept for simcap's life: the capability lives in it.
    let storage = UnsafeMutableRawPointer.allocate(
      byteCount: MemoryLayout<any CoreDeviceOpaqueCapability>.size,
      alignment: MemoryLayout<any CoreDeviceOpaqueCapability>.alignment)
    let emptyStaticMember = UnsafeMutableRawPointer.allocate(byteCount: 1, alignment: 1)
    defer { emptyStaticMember.deallocate() }
    // CapabilityStaticMember<T> is empty but resilient: its generic method
    // takes an address, the actual T's metadata, and its conformance.
    do {
      try await device.implementation(storage, emptyStaticMember, metadata, witness)
    } catch {
      storage.deallocate()
      throw error
    }
    return storage
  }

  private static func remoteDevice(udid: String) async throws -> CoreDeviceRemoteDevice {
    guard SSCoreDeviceInitialize() else { throw Failure.unavailable }
    if manager == nil {
      // Initializes resilient class metadata and field offsets before the
      // class metadata's allocating initializer is used.
      _ = coreDeviceSharedManager()
      guard let managerMetadata = SSCoreDeviceSymbol("$s10CoreDevice0B7ManagerCN"),
            let visibilityMetadata = SSCoreDeviceSymbol("$s10CoreDevice0B15VisibilityClassON"),
            let visibilityType = unsafeBitCast(visibilityMetadata, to: Any.Type.self) as? any Hashable.Type
      else { throw Failure.unavailable }
      let managerType = unsafeBitCast(managerMetadata, to: CoreDeviceManagerObject.Type.self)
      let visibilitySet = makeVisibilitySet(visibilityType)
      manager = withExtendedLifetime(visibilitySet) {
        let rawSet = withUnsafePointer(to: visibilitySet) { UnsafeRawPointer($0).load(as: UnsafeMutableRawPointer.self) }
        // The private initializer consumes the Set; the Any keeps the original alive.
        SSCoreDeviceRetainBridgeObject(rawSet)
        return managerType.create(connection: coreDeviceServiceConnection(), allowed: rawSet)
      }
    }
    guard let manager else { throw Failure.unavailable }
    // The manager learns the devices after it is made; five seconds is plenty.
    let deadline = ContinuousClock.now + .seconds(5)
    while !manager.initialized() {
      guard ContinuousClock.now < deadline else { throw Failure.notInitialized }
      try await Task.sleep(for: .milliseconds(50))
    }
    guard let device = manager.allDevices().first(where: {
      $0.identifier().uuidString.caseInsensitiveCompare(udid) == .orderedSame
    }) else { throw Failure.noDevice }
    return device
  }

  /// The display CoreDevice marks active, and its framebuffer's turn in
  /// degrees, read from Apple's own Display values at their real type.
  static func activeDisplay(udid: String) async throws -> (screen: UInt32, rotation: Int32)? {
    guard SSCoreDeviceDisplayAvailable(), let infoMetadata = SSCoreDeviceDisplayInfoMetadata(),
          let displayMetadata = SSCoreDeviceDisplayMetadata()
    else { return nil }
    let device = try await remoteDevice(udid: udid)
    let info = UnsafeMutableRawPointer.allocate(byteCount: Int(SSCoreDeviceValueSize(infoMetadata)), alignment: 16)
    defer { info.deallocate() }
    try await device.displayInfo(info)
    defer { SSCoreDeviceDestroyValue(info, infoMetadata) }
    func active<T>(_: T.Type) -> (screen: UInt32, rotation: Int32)? {
      guard let array = SSCoreDeviceDisplays(info) else { return nil }
      for value in unsafeBitCast(array, to: [T].self) {
        var value = value
        let found: (UInt32, Int32)? = withUnsafeMutablePointer(to: &value) { pointer in
          guard SSCoreDeviceDisplayActive(pointer) == 1 else { return nil }
          let orientation = unsafeBitCast(SSCoreDeviceDisplayOrientation(pointer), to: String.self)
          // "rot0", "rot90", "rot180", "rot270".
          let degrees = Int32(orientation.dropFirst(3)) ?? 0
          return (SSCoreDeviceDisplayID(pointer), degrees)
        }
        if let found { return found }
      }
      return nil
    }
    return _openExistential(unsafeBitCast(displayMetadata, to: Any.Type.self), do: active)
  }

  /// Apple's own enum's allCases as a Set, opened at its real type rather
  /// than a substitute's tags or hashes.
  private static func makeVisibilitySet<T: Hashable>(_: T.Type) -> Any {
    Set(unsafeBitCast(coreDeviceVisibilityAllCases(), to: [T].self))
  }
}

// These supply the Swift calling convention only: objects and protocol
// metadata always come from CoreDevice, and no instance of these is made.
private protocol CoreDeviceServiceConnection {}
private protocol CoreDeviceOpaqueCapability {}

@_silgen_name("SSCDShared")
private func coreDeviceSharedManager() -> CoreDeviceManagerObject
@_silgen_name("SSCDConnection")
private func coreDeviceServiceConnection() -> any CoreDeviceServiceConnection
@_silgen_name("SSCDVisibilityAllCases")
private func coreDeviceVisibilityAllCases() -> UnsafeRawPointer

private class CoreDeviceManagerObject {
  @_silgen_name("SSCDCreateManager")
  static func create(connection: __owned any CoreDeviceServiceConnection, allowed: UnsafeRawPointer)
    -> CoreDeviceManagerObject
  @_silgen_name("SSCDAllDevices")
  final func allDevices() -> [CoreDeviceRemoteDevice]
  @_silgen_name("SSCDInitialized")
  final func initialized() -> Bool
}

private class CoreDeviceRemoteDevice {
  @_silgen_name("SSCDIdentifier")
  final func identifier() -> UUID
  @_silgen_name("SSCDImplementation")
  final func implementation(_ result: UnsafeMutableRawPointer, _ empty: UnsafeRawPointer,
                            _ metadata: UnsafeRawPointer, _ witness: UnsafeRawPointer) async throws
  @_silgen_name("SSCDDisplayInfo")
  final func displayInfo(_ result: UnsafeMutableRawPointer) async throws
}
