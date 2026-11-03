;; aggstrbytes -- sums the bytes of every string value it is handed.
;;
;; It exists to make the host's STRING ARENA observable. A string slot carries a
;; length and a pointer into the arena the host laid out after the value slots,
;; and nothing else in this suite reads either: a host that wrote the wrong
;; pointer, or that reused one record's bytes for the next, would be invisible to
;; a fixture that only ever adds numbers. Summing the bytes rather than the
;; length checks both halves -- a wrong length gives a wrong total, and so does a
;; right length over the wrong bytes.
;;
;; State is one i64.
(module
  (memory (export "memory") 1)

  (func (export "classad_udaf_abi_version") (result i32) (i32.const 1))

  (func $init (param $out i32) (param $cap i32) (result i32)
    (if (i32.lt_u (local.get $cap) (i32.const 8))
      (then (return (i32.const -1))))
    (i64.store (local.get $out) (i64.const 0))
    (i32.const 8))

  (func $fold (param $sp i32) (param $sl i32) (param $vp i32) (param $n i32)
              (param $op i32) (param $oc i32) (result i32)
    (local $i i32) (local $slot i32) (local $ptr i32) (local $end i32)
    (local $sum i64)
    (if (i32.ne (local.get $sl) (i32.const 8))
      (then (return (i32.const -1))))
    (if (i32.lt_u (local.get $oc) (i32.const 8))
      (then (return (i32.const -1))))
    (local.set $sum (i64.load (local.get $sp)))
    (block $done
      (loop $next
        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $slot
          (i32.add (local.get $vp) (i32.mul (local.get $i) (i32.const 16))))
        (if (i32.eq (i32.load (local.get $slot)) (i32.const 5))
          (then
            ;; The payload is an absolute offset in this memory; the length is
            ;; the second word.
            (local.set $ptr (i32.wrap_i64 (i64.load offset=8 (local.get $slot))))
            (local.set $end
              (i32.add (local.get $ptr) (i32.load offset=4 (local.get $slot))))
            (block $bdone
              (loop $bnext
                (br_if $bdone (i32.ge_u (local.get $ptr) (local.get $end)))
                (local.set $sum
                  (i64.add (local.get $sum)
                           (i64.load8_u (local.get $ptr))))
                (local.set $ptr (i32.add (local.get $ptr) (i32.const 1)))
                (br $bnext)))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $next)))
    (i64.store (local.get $op) (local.get $sum))
    (i32.const 8))

  (func $merge (param $ap i32) (param $al i32) (param $bp i32) (param $bl i32)
               (param $op i32) (param $oc i32) (result i32)
    (if (i32.lt_u (local.get $oc) (i32.const 8))
      (then (return (i32.const -1))))
    (i64.store (local.get $op)
      (i64.add (i64.load (local.get $ap)) (i64.load (local.get $bp))))
    (i32.const 8))

  (func $result (param $sp i32) (param $sl i32) (param $rp i32) (result i32)
    (i32.store (local.get $rp) (i32.const 3))
    (i32.store offset=4 (local.get $rp) (i32.const 0))
    (i64.store offset=8 (local.get $rp) (i64.load (local.get $sp)))
    (i32.const 0))

  (export "classad_udaf_init" (func $init))
  (export "classad_udaf_fold" (func $fold))
  (export "classad_udaf_merge" (func $merge))
  (export "classad_udaf_result" (func $result))
)
