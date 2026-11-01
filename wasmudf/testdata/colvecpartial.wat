;; colvecpartial -- describes a whole column and then fills only the first half of
;; it, declining nothing.
;;
;; This is the hole the columnar result opens and the fixture that measures it.
;; Version 1 poisons a state byte per element, so this guest would be caught at
;; the first element it skipped; one state byte for the whole column cannot catch
;; it. What the host does instead is zero the payload array before every call, so
;; the half this guest never writes is zeros rather than whatever the PREVIOUS
;; batch computed for other records -- a wrong answer either way, but one that
;; does not depend on what was asked before it.
(module
  (memory (export "memory") 1)

  (func (export "classad_udf_abi_version") (result i32) (i32.const 2))

  (func (export "classad_udf_call_vec")
        (param $n i32) (param $args i32) (param $argc i32)
        (param $res i32) (param $decl i32) (result i32)
    (local $i i32) (local $limit i32) (local $iPtr i32) (local $rI i32) (local $off i32)
    (local.set $iPtr (i32.load offset=12 (local.get $args)))
    (local.set $rI (i32.load offset=12 (local.get $res)))
    ;; How much of the column it fills depends on the data: a negative first
    ;; element and it writes all of it, otherwise only the first half. That is
    ;; what lets a test put a full column in the buffer and then ask what the
    ;; NEXT batch sees in the part it did not write.
    (local.set $limit (i32.shr_u (local.get $n) (i32.const 1)))
    (if (i64.lt_s (i64.load (local.get $iPtr)) (i64.const 0))
      (then (local.set $limit (local.get $n))))
    (i32.store8 (i32.load offset=8 (local.get $res)) (i32.const 0))
    (block $end (loop $next
      (br_if $end (i32.ge_u (local.get $i) (local.get $limit)))
      (local.set $off (i32.shl (local.get $i) (i32.const 3)))
      (i64.store (i32.add (local.get $rI) (local.get $off))
        (i64.mul (i64.load (i32.add (local.get $iPtr) (local.get $off))) (i64.const 2)))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $next)))
    (i32.const 0))

  (func (export "classad_udf_call")
        (param $a i32) (param $argc i32) (param $res i32) (result i32)
    (i32.store (local.get $res) (i32.const 3))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res)
      (i64.mul (i64.load offset=8 (local.get $a)) (i64.const 2)))
    (i32.const 0))
)
