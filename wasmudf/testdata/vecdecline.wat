;; vecdecline -- declines every record whose value is ODD, and leaves GARBAGE in
;; the slots of the records it declined: state 7, which is not a value at all,
;; over the payload 999.
;;
;; The garbage is the point, and it has to be garbage rather than a wrong number.
;; "I declined this record" means "do not look", so a guest is entitled to leave
;; anything in that slot -- it may be a scratch area, or simply untouched. A host
;; that read declined slots would therefore reject this whole batch as malformed,
;; even though every record the guest actually claimed is perfect. (Writing a
;; plausible 999 instead does NOT test that: the escalation overwrites the slot
;; afterwards, so reading it first makes no difference to the answer.)
;;
;; Declining by VALUE rather than by index is also deliberate: it makes the set of
;; declined records change from batch to batch, which is what a test needs to see
;; whether the host clears the decline mask between calls.
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))

  (func $setdecl (param $decl i32) (param $i i32)
    (local $w i32)
    (local.set $w (i32.add (local.get $decl)
                           (i32.shl (i32.shr_u (local.get $i) (i32.const 6)) (i32.const 3))))
    (i64.store (local.get $w)
      (i64.or (i64.load (local.get $w))
              (i64.shl (i64.const 1)
                       (i64.extend_i32_u (i32.and (local.get $i) (i32.const 63)))))))

  (func (export "classad_udf_call_vec")
        (param $n i32) (param $args i32) (param $argc i32)
        (param $res i32) (param $decl i32) (result i32)
    (local $stPtr i32) (local $iPtr i32)
    (local $rSt i32) (local $rI i32)
    (local $i i32) (local $off i32) (local $v i64)

    (if (i32.ne (local.get $argc) (i32.const 1))
      (then (return (i32.const 1))))
    (local.set $stPtr (i32.load offset=8 (local.get $args)))
    (local.set $iPtr (i32.load offset=12 (local.get $args)))
    (local.set $rSt (i32.load offset=8 (local.get $res)))
    (local.set $rI (i32.load offset=12 (local.get $res)))

    (block $end
      (loop $next
        (br_if $end (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $off (i32.shl (local.get $i) (i32.const 3)))
        (local.set $v (i64.load (i32.add (local.get $iPtr) (local.get $off))))
        (if (i32.or
              (i32.ne (i32.load8_u (i32.add (local.get $stPtr) (local.get $i))) (i32.const 0))
              (i64.ne (i64.and (local.get $v) (i64.const 1)) (i64.const 0)))
          (then
            (call $setdecl (local.get $decl) (local.get $i))
            ;; Not a value, on purpose, in a slot nobody may read.
            (i32.store8 (i32.add (local.get $rSt) (local.get $i)) (i32.const 7))
            (i64.store (i32.add (local.get $rI) (local.get $off)) (i64.const 999)))
          (else
            (i32.store8 (i32.add (local.get $rSt) (local.get $i)) (i32.const 0))
            (i64.store (i32.add (local.get $rI) (local.get $off))
                       (i64.mul (local.get $v) (i64.const 2)))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $next)))
    (i32.const 0))

  (func (export "classad_udf_call")
        (param $a i32) (param $argc i32) (param $res i32) (result i32)
    (if (i32.ne (i32.load (local.get $a)) (i32.const 3))
      (then
        ;; Pass UNDEFINED and ERROR through; refuse anything else.
        (if (i32.le_u (i32.load (local.get $a)) (i32.const 1))
          (then
            (i32.store (local.get $res) (i32.load (local.get $a)))
            (i32.store offset=4 (local.get $res) (i32.const 0))
            (i64.store offset=8 (local.get $res) (i64.const 0))
            (return (i32.const 0))))
        (return (i32.const 2))))
    (i32.store (local.get $res) (i32.const 3))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res)
      (i64.mul (i64.load offset=8 (local.get $a)) (i64.const 2)))
    (i32.const 0))
)
