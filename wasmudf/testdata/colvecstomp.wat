;; colvecstomp -- OVERWRITES the decline mask instead of ORing into it, and then
;; computes every lane as though it were an integer.
;;
;; Version 1 handed a guest a mask that was always zero, so storing into it rather
;; than ORing was legal; a guest written that way and recompiled against version 2
;; would wipe the host's own declines. The host ORs them back for exactly this
;; reason, and this fixture is what makes that a test: the records whose value was
;; not an integer still get their scalar answer, even though this guest claimed
;; them.
(module
  (memory (export "memory") 1)

  (func (export "classad_udf_abi_version") (result i32) (i32.const 2))

  (func (export "classad_udf_call_vec")
        (param $n i32) (param $args i32) (param $argc i32)
        (param $res i32) (param $decl i32) (result i32)
    (local $i i32) (local $w i32) (local $words i32) (local $iPtr i32) (local $rI i32)
    (local.set $words (i32.shr_u (i32.add (local.get $n) (i32.const 63)) (i32.const 6)))
    (block $endw (loop $nextw
      (br_if $endw (i32.ge_u (local.get $w) (local.get $words)))
      (i64.store (i32.add (local.get $decl) (i32.shl (local.get $w) (i32.const 3)))
                 (i64.const 0))
      (local.set $w (i32.add (local.get $w) (i32.const 1)))
      (br $nextw)))
    (local.set $iPtr (i32.load offset=12 (local.get $args)))
    (local.set $rI (i32.load offset=12 (local.get $res)))
    (i32.store8 (i32.load offset=8 (local.get $res)) (i32.const 0))
    (block $end (loop $next
      (br_if $end (i32.ge_u (local.get $i) (local.get $n)))
      (i64.store (i32.add (local.get $rI) (i32.shl (local.get $i) (i32.const 3)))
        (i64.mul (i64.load (i32.add (local.get $iPtr) (i32.shl (local.get $i) (i32.const 3))))
                 (i64.const 2)))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $next)))
    (i32.const 0))

  ;; The scalar entry point passes UNDEFINED and ERROR through, so a record the
  ;; host declined is visible in the answer as the state it actually had rather
  ;; than as an integer this guest invented for it.
  (func (export "classad_udf_call")
        (param $a i32) (param $argc i32) (param $res i32) (result i32)
    (local $tag i32)
    (local.set $tag (i32.load (local.get $a)))
    (i32.store (local.get $res) (local.get $tag))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res) (i64.const 0))
    (if (i32.eq (local.get $tag) (i32.const 3))
      (then
        (i64.store offset=8 (local.get $res)
          (i64.mul (i64.load offset=8 (local.get $a)) (i64.const 2)))))
    (i32.const 0))
)
