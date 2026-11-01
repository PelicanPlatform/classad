;; concat -- a two-argument string function that builds its result in memory it
;; owns rather than echoing a host pointer. It writes at offset 1024, below the
;; host's region (which the host places in pages it added after instantiation), so
;; the two never overlap -- the property the region placement exists to guarantee.
(module
  (memory (export "memory") 1)

  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))

  (func (export "classad_udf_call")
        (param $args i32) (param $argc i32) (param $res i32) (result i32)
    (local $alen i32)
    (local $blen i32)
    (if (i32.ne (local.get $argc) (i32.const 2))
      (then (return (i32.const 1))))
    (if (i32.or (i32.ne (i32.load (local.get $args)) (i32.const 5))
                (i32.ne (i32.load offset=16 (local.get $args)) (i32.const 5)))
      (then (return (i32.const 1))))
    (local.set $alen (i32.load offset=4 (local.get $args)))
    (local.set $blen (i32.load offset=20 (local.get $args)))
    (memory.copy (i32.const 1024)
                 (i32.wrap_i64 (i64.load offset=8 (local.get $args)))
                 (local.get $alen))
    (memory.copy (i32.add (i32.const 1024) (local.get $alen))
                 (i32.wrap_i64 (i64.load offset=24 (local.get $args)))
                 (local.get $blen))
    (i32.store (local.get $res) (i32.const 5))
    (i32.store offset=4 (local.get $res) (i32.add (local.get $alen) (local.get $blen)))
    (i64.store offset=8 (local.get $res) (i64.const 1024))
    (i32.const 0))
)
